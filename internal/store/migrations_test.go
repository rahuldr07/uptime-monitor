package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func rawCount(t *testing.T, db *sql.DB, q string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMigrateFreshDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := userVersion(t, s); v != len(steps) || v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'daily'`); n != 1 {
		t.Fatal("daily table missing on fresh database")
	}
	s.Close()

	raw := rawOpen(t, path)
	if _, err := raw.Exec(`DROP TABLE daily`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'daily'`); n != 0 {
		t.Fatal("daily was recreated: step 1 ran again at version 1")
	}
}

func TestMigrateLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	raw := rawOpen(t, path)
	if _, err := raw.Exec(schema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := raw.Exec(`INSERT INTO targets (name, url, interval_ms, timeout_ms, created_at) VALUES ('a', 'http://x', 1000, 1000, ?)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO checks (target_id, checked_at, ok, status_code, latency_ms) VALUES (1, ?, 1, 200, 50), (1, ?, 0, 0, 0)`, now, now-dayMS); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := userVersion(t, s); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	ctx := context.Background()
	for _, c := range [][2]string{{"targets", "grp"}, {"targets", "muted"}, {"incidents", "acked_at"}, {"incidents", "note"}, {"checks", "maint"}} {
		has, err := hasColumn(ctx, s.db, c[0], c[1])
		if err != nil || !has {
			t.Fatalf("hasColumn(%s.%s) = %v, %v", c[0], c[1], has, err)
		}
	}
	if _, err := s.GetTarget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	st, err := s.RollupStats(ctx, 1, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 2 || st.OKCount != 1 {
		t.Fatalf("stats = %+v, want Total 2 OKCount 1", st)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "newer than this build supports") {
		t.Fatalf("Open err = %v, want newer-schema error", err)
	}
	raw := rawOpen(t, path)
	defer raw.Close()
	if v := rawCount(t, raw, `PRAGMA user_version`); v != 99 {
		t.Fatalf("user_version = %d, want 99", v)
	}
}

func TestMigrateStepIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	orig := steps
	t.Cleanup(func() { steps = orig })
	steps = append(append([]func(context.Context, *sql.Tx) error{}, orig...), func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE half_done(x)`); err != nil {
			return err
		}
		return errors.New("boom")
	})
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Open err = %v, want boom", err)
	}
	raw := rawOpen(t, path)
	if v := rawCount(t, raw, `PRAGMA user_version`); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'half_done'`); n != 0 {
		t.Fatal("half_done table survived a failed step")
	}
	raw.Close()

	steps = orig
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}
