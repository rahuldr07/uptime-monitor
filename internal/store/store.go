// Package store persists targets, checks, incidents and alerts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const dayMS = 86400000

// Open opens or creates the database at path and ensures the schema exists.
// The file holds alert credentials, so it is created readable by the owner only.
func Open(path string) (*Store, error) {
	if err := restrictPerms(path); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite allows one writer at a time; a single connection keeps things simple.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// restrictPerms creates the database file with mode 0600, or tightens an
// existing file and its WAL/SHM companions to that mode.
func restrictPerms(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open database file: %w", err)
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm()&0o077 == 0 {
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return fmt.Errorf("restrict permissions on %s: %w", p, err)
		}
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Backup writes a consistent copy of the database at src to dest. It is safe
// to run while the monitor is using the database. The source is opened
// read-only and must already exist, so a wrong path fails loudly instead of
// producing an empty "backup". dest must not exist; it is created with mode
// 0600 before any data is written, since it contains alert credentials.
func Backup(ctx context.Context, src, dest string) (err error) {
	fi, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("backup: source database: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("backup: source %s is not a regular file", src)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	f.Close()
	defer func() {
		if err != nil {
			os.Remove(dest)
		}
	}()
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("backup: open source: %w", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'targets'`).Scan(&n); err != nil {
		return fmt.Errorf("backup: read source: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("backup: %s is not an uptime-monitor database", src)
	}
	// VACUUM INTO accepts an existing empty file and keeps its permissions.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// Ping checks that the database answers a query.
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one)
}

// ---- helpers ----

type scanner interface {
	Scan(dest ...any) error
}

func requireRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
