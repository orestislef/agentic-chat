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

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies pending migrations.
func Open(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=auto_vacuum(INCREMENTAL)" + // only takes effect on a new file, has to come first
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" + // an acknowledged message survives power loss
		"&_pragma=foreign_keys(1)" +
		"&_pragma=journal_size_limit(67108864)" + // the WAL file shrinks back to 64 MB after a checkpoint
		"&_pragma=cache_size(-8000)" +
		"&_pragma=temp_store(MEMORY)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// WAL allows many readers next to one writer, the busy timeout queues writers.
	db.SetMaxOpenConns(8)
	s := &Store{db: db}
	if err := s.checkIntegrity(context.Background(), path); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
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
		rows, err := s.db.QueryContext(ctx, step.query, step.args...)
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

// Close releases the database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}
	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := current; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
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
