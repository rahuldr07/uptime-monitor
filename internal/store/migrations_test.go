package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
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
	if v := userVersion(t, s); v != len(steps) || v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
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
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	ctx := context.Background()
	for _, c := range [][2]string{{"targets", "grp"}, {"targets", "muted"}, {"incidents", "acked_at"}, {"incidents", "note"}, {"checks", "maint"}} {
		has, err := hasColumn(ctx, s.db, c[0], c[1])
		if err != nil || !has {
			t.Fatalf("hasColumn(%s.%s) = %v, %v", c[0], c[1], has, err)
		}
	}
	if _, err := s.Workspace(1).GetTarget(ctx, 1); err != nil {
		t.Fatal(err)
	}
	st, err := s.Workspace(1).RollupStats(ctx, 1, time.Now().Add(-7*24*time.Hour))
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
	steps = append(append([]migration{}, orig...), migration{run: func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE half_done(x)`); err != nil {
			return err
		}
		return errors.New("boom")
	}})
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Open err = %v, want boom", err)
	}
	raw := rawOpen(t, path)
	if v := rawCount(t, raw, `PRAGMA user_version`); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
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

// openV1 builds a v1 database at path holding targets A and B (B deleted),
// their history, a maintenance window and a setting.
func openV1(t *testing.T, path string) (deletedID int64) {
	t.Helper()
	orig := steps
	steps = orig[:1]
	defer func() { steps = orig }()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, q := range []string{
		`INSERT INTO targets (name, url, interval_ms, timeout_ms, created_at) VALUES ('A', 'http://a', 1000, 1000, 1), ('B', 'http://b', 1000, 1000, 1)`,
		`INSERT INTO checks (target_id, checked_at, ok, status_code, latency_ms) VALUES (1, 1, 1, 200, 5), (2, 1, 1, 200, 5)`,
		`INSERT INTO incidents (target_id, started_at) VALUES (1, 1)`,
		`INSERT INTO alerts (incident_id, target_id, sent_at, channel, event, ok) VALUES (1, 1, 1, 'slack', 'down', 1)`,
		`INSERT INTO maintenance (title, starts_at, ends_at, created_at) VALUES ('m', 1, 2, 1)`,
		`INSERT INTO settings (key, value) VALUES ('notify', '{}')`,
		`DELETE FROM targets WHERE name = 'B'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return 2
}

func TestMigrateV2Upgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	bID := openV1(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	var id int64
	var name, slug string
	if err := s.db.QueryRow(`SELECT id, name, slug FROM workspaces`).Scan(&id, &name, &slug); err != nil || id != 1 || name != "Default" || slug != "default" {
		t.Fatalf("workspace = %d %q %q, %v", id, name, slug, err)
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM workspaces`); n != 1 {
		t.Fatalf("workspaces = %d, want 1", n)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM targets WHERE name = 'A' AND workspace_id = 1`,
		`SELECT COUNT(*) FROM maintenance WHERE workspace_id = 1`,
		`SELECT COUNT(*) FROM settings WHERE workspace_id = 1`,
	} {
		if n := rawCount(t, s.db, q); n != 1 {
			t.Fatalf("%s = %d, want 1", q, n)
		}
	}
	ctx := context.Background()
	if v, err := s.Workspace(1).GetSetting(ctx, "notify"); err != nil || v != "{}" {
		t.Fatalf("GetSetting = %q, %v", v, err)
	}
	for table, want := range map[string]int{"checks": 2, "incidents": 1, "alerts": 1} {
		if n := rawCount(t, s.db, `SELECT COUNT(*) FROM `+table); n != want {
			t.Fatalf("%s rows = %d, want %d", table, n, want)
		}
	}
	nt, err := s.Workspace(1).CreateTarget(ctx, Target{Name: "C", URL: "http://c", Interval: time.Second, Timeout: time.Second}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if nt.ID <= bID {
		t.Fatalf("new target id = %d, want > %d", nt.ID, bID)
	}
	rows, err := s.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("foreign_key_check returned rows")
	}
	rows.Close()
	if n := rawCount(t, s.db, `PRAGMA foreign_keys`); n != 1 {
		t.Fatalf("foreign_keys = %d, want 1", n)
	}
	s.Close()

	baks, _ := filepath.Glob(path + ".pre-accounts-*.bak")
	if len(baks) != 1 {
		t.Fatalf("backups = %v, want 1", baks)
	}
	if fi, err := os.Stat(baks[0]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode = %v, %v", fi, err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	if baks, _ := filepath.Glob(path + ".pre-accounts-*.bak"); len(baks) != 1 {
		t.Fatalf("backups = %v, want 1", baks)
	}
}

func TestMigrateV2Fresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM workspaces WHERE id = 1`); n != 1 {
		t.Fatal("workspace 1 missing")
	}
	if baks, _ := filepath.Glob(filepath.Join(dir, "*.bak")); len(baks) != 0 {
		t.Fatalf("unexpected backups %v", baks)
	}
}

func TestMigrateV2FailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	openV1(t, path)

	orig := steps
	t.Cleanup(func() { steps = orig })
	steps = append([]migration{}, orig...)
	steps[1].run = func(ctx context.Context, tx *sql.Tx) error {
		if err := v2(ctx, tx); err != nil {
			return err
		}
		return errors.New("boom")
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Open err = %v, want boom", err)
	}
	raw := rawOpen(t, path)
	if v := rawCount(t, raw, `PRAGMA user_version`); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'workspaces'`); n != 0 {
		t.Fatal("workspaces table survived a failed step")
	}
	if has, err := hasColumn(context.Background(), raw, "targets", "workspace_id"); err != nil || has {
		t.Fatalf("targets.workspace_id = %v, %v", has, err)
	}
	if n := rawCount(t, raw, `SELECT COUNT(*) FROM targets`); n != 1 {
		t.Fatalf("targets = %d, want 1", n)
	}
	raw.Close()

	steps = orig
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if n := rawCount(t, s.db, `PRAGMA foreign_keys`); n != 1 {
		t.Fatalf("foreign_keys = %d, want 1", n)
	}
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
}

func TestMigrateRetryReusesBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	openV1(t, path)

	orig := steps
	t.Cleanup(func() { steps = orig })
	steps = append([]migration{}, orig...)
	steps[1].run = func(ctx context.Context, tx *sql.Tx) error {
		if err := v2(ctx, tx); err != nil {
			return err
		}
		return errors.New("boom")
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Open err = %v, want boom", err)
	}
	steps = orig
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	baks, err := filepath.Glob(path + ".pre-accounts-*.bak")
	if err != nil || len(baks) != 1 {
		t.Fatalf("backups = %v, %v; want exactly one", baks, err)
	}
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
}

func baks(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMigrateBackupPathWithGlobChars(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "[x")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "t.db")
	openV1(t, path)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".bak") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("backups = %d, want 1", n)
	}
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
}

func TestMigrateReplacesEmptyBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	openV1(t, path)
	empty := path + ".pre-accounts-20000101-000000.bak"
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatalf("empty backup should be removed: %v", err)
	}
	b := baks(t, dir)
	if len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if fi, err := os.Stat(b[0]); err != nil || fi.Size() == 0 {
		t.Fatalf("backup empty: %v", err)
	}
	if v := userVersion(t, s); v != 2 {
		t.Fatalf("user_version = %d, want 2", v)
	}
}

func TestMigrateIgnoresOtherDatabasesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.db")
	openV1(t, path)
	other := filepath.Join(dir, "other.db.pre-accounts-20000101-000000.bak")
	if err := os.WriteFile(other, []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	own, err := filepath.Glob(filepath.Join(dir, "t.db.pre-accounts-*.bak"))
	if err != nil || len(own) != 1 {
		t.Fatalf("own backups = %v, %v; want one", own, err)
	}
	if len(baks(t, dir)) != 2 {
		t.Fatalf("backups = %v", baks(t, dir))
	}
}

func TestTwoWorkspacesSameTargetName(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.Exec(`INSERT INTO workspaces (id, name, slug, created_at) VALUES (2, 'Two', 'two', 1)`); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO targets (workspace_id, name, url, interval_ms, timeout_ms, created_at) VALUES (?, 'X', 'http://x', 1000, 1000, 1)`
	for _, ws := range []int{1, 2} {
		if _, err := s.db.Exec(ins, ws); err != nil {
			t.Fatalf("workspace %d: %v", ws, err)
		}
	}
	if _, err := s.db.Exec(ins, 1); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate err = %v, want UNIQUE", err)
	}
}
