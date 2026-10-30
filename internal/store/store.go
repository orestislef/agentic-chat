// Package store keeps all hub state in a single SQLite file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist or is not visible to the caller.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a unique value such as a name is already taken.
var ErrConflict = errors.New("already exists")

// Store wraps the database. SQLite allows one writer at a time, and writers that compete
// for the lock spend their time sleeping and retrying. So all writes share one connection,
// and frequent small ones are grouped into a single commit by writeLoop. Reads run in
// parallel on their own connections and never wait for a write.
type Store struct {
	db     *sql.DB // reads
	w      *sql.DB // writes, exactly one connection
	writes chan writeOp
	stop   chan struct{}
	done   chan struct{}
	demo   bool
}

type writeOp struct {
	fn     func(*sql.Tx) error
	result chan error
}

// maxBatch caps how many writes share one commit, so a burst cannot make it arbitrarily large.
const maxBatch = 256

// ErrClosed is returned for writes that arrive while the store shuts down.
var ErrClosed = errors.New("store is closed")

// SetDemo marks everything created from now on as demo data, except for admins.
func (s *Store) SetDemo(on bool) {
	s.demo = on
}

// Open opens or creates the database at path and applies pending migrations.
func Open(path string) (*Store, error) {
	file := "file:" + filepath.ToSlash(path)
	w, err := sql.Open("sqlite", file+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=auto_vacuum(INCREMENTAL)"+ // only takes effect on a new file, has to come first
		"&_pragma=journal_mode(WAL)"+
		"&_pragma=synchronous(FULL)"+ // an acknowledged message survives power loss
		"&_pragma=foreign_keys(1)"+
		"&_pragma=journal_size_limit(67108864)"+ // the WAL file shrinks back to 64 MB after a checkpoint
		"&_pragma=cache_size(-8000)"+
		"&_pragma=temp_store(MEMORY)"+
		"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	// Readers only set what they need. The rest is stored in the file by the writer, and
	// setting it again on each new connection can run into the write lock.
	db, err := sql.Open("sqlite", file+
		"?_pragma=busy_timeout(5000)"+
		"&_pragma=cache_size(-8000)"+
		"&_pragma=temp_store(MEMORY)")
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8) // reopening means running the pragmas again
	s := &Store{db: db, w: w, writes: make(chan writeOp, maxBatch), stop: make(chan struct{}), done: make(chan struct{})}
	// Touching the writer first creates the file with the right settings.
	if err := w.PingContext(context.Background()); err != nil {
		db.Close()
		w.Close()
		return nil, fmt.Errorf("database %s cannot be read, it may be damaged, restore a copy from the backups folder: %w", path, err)
	}
	if err := s.checkIntegrity(context.Background(), path); err != nil {
		db.Close()
		w.Close()
		return nil, err
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		w.Close()
		return nil, err
	}
	go s.writeLoop()
	return s, nil
}

// write runs fn inside a transaction shared with other waiting writes and returns once it
// is durably committed. A lone write commits right away, under load many share one disk sync.
// A cancelled caller stops waiting, but its write still happens: it may already be part of a
// batch, and the other writes in there must not fail because of it.
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	op := writeOp{fn: fn, result: make(chan error, 1)}
	select {
	case s.writes <- op:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.stop:
		return ErrClosed
	}
	select {
	case err := <-op.result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) writeLoop() {
	defer close(s.done)
	batch := make([]writeOp, 0, maxBatch)
	for {
		select {
		case op := <-s.writes:
			batch = append(batch[:0], op)
		case <-s.stop:
			s.failPending()
			return
		}
	drain:
		for len(batch) < maxBatch {
			select {
			case op := <-s.writes:
				batch = append(batch, op)
			default:
				break drain
			}
		}
		s.commit(batch)
	}
}

func (s *Store) failPending() {
	for {
		select {
		case op := <-s.writes:
			op.result <- ErrClosed
		default:
			return
		}
	}
}

// commit runs a batch. Each write gets a savepoint, so one that fails is undone alone.
func (s *Store) commit(batch []writeOp) {
	ctx := context.Background()
	results := make([]error, len(batch))
	tx, err := s.w.BeginTx(ctx, nil)
	if err == nil {
		for i, op := range batch {
			if _, err := tx.ExecContext(ctx, `SAVEPOINT op`); err != nil {
				results[i] = err
				continue
			}
			results[i] = op.fn(tx)
			if results[i] != nil {
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO op`); err != nil {
					results[i] = errors.Join(results[i], err)
				}
			}
			if _, err := tx.ExecContext(ctx, `RELEASE op`); err != nil {
				results[i] = errors.Join(results[i], err)
			}
		}
		err = tx.Commit()
	}
	for i, op := range batch {
		if err != nil {
			results[i] = err
		}
		op.result <- results[i]
	}
}

// checkIntegrity refuses to run on a damaged file. Serving from one would keep
// writing to it and could make things worse, stopping leaves the choice to the operator.
func (s *Store) checkIntegrity(ctx context.Context, path string) error {
	var result string
	if err := s.db.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&result); err != nil {
		return fmt.Errorf("database %s cannot be read, it may be damaged, restore a copy from the backups folder: %w", path, err)
	}
	if result != "ok" {
		return fmt.Errorf("database %s is damaged (%s), restore a copy from the backups folder", path, result)
	}
	return nil
}

// Backup writes a consistent copy of the live database to path. It goes through a
// temporary file, so an interrupted backup never looks like a finished one.
func (s *Store) Backup(ctx context.Context, path string) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	// On a read connection, because copying a big file must not hold up writers.
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("back up database: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("back up database: %w", err)
	}
	return nil
}

// Maintain cleans up what nothing else removes, and gives freed space back to the
// disk. It is meant to run about once an hour.
func (s *Store) Maintain(ctx context.Context) error {
	now := nowMillis()
	steps := []struct {
		query string
		args  []any
	}{
		{`DELETE FROM sessions WHERE expires_at <= ?`, []any{now}},
		{`DELETE FROM invites WHERE expires_at <= ? OR uses >= max_uses`, []any{now}},
		// Retry keys only matter for minutes, a day is generous.
		{`UPDATE messages SET client_key = NULL WHERE client_key IS NOT NULL AND created_at < ?`, []any{now - 24*60*60*1000}},
		// A revoked agent stays while history still names it, then it can go.
		{`DELETE FROM agents WHERE revoked_at IS NOT NULL
			AND NOT EXISTS (SELECT 1 FROM messages WHERE from_agent_id = agents.id)
			AND NOT EXISTS (SELECT 1 FROM messages WHERE to_agent_id = agents.id)`, nil},
		{`PRAGMA optimize`, nil},
		{`PRAGMA incremental_vacuum(2000)`, nil},
		{`PRAGMA wal_checkpoint(TRUNCATE)`, nil},
	}
	for _, step := range steps {
		// A query rather than an exec, because the pragmas return rows and
		// incremental_vacuum only does its work as those rows are read.
		rows, err := s.w.QueryContext(ctx, step.query, step.args...)
		if err != nil {
			return fmt.Errorf("maintenance: %w", err)
		}
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("maintenance: %w", err)
		}
	}
	return nil
}

// Close finishes running writes and releases the database.
func (s *Store) Close() error {
	close(s.stop)
	<-s.done
	return errors.Join(s.w.Close(), s.db.Close())
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.w.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var current int
	if err := s.w.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, i+1); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowMillis() int64 {
	return time.Now().UnixMilli()
}

func rowErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// nullMillis converts a nullable millisecond column to a plain value, zero meaning unset.
func nullMillis(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}
