// Package store keeps all hub state in a single SQLite file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// WAL allows many readers next to one writer, the busy timeout queues writers.
	db.SetMaxOpenConns(8)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
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
