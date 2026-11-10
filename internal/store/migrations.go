package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const schema = `
CREATE TABLE IF NOT EXISTS targets (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	name          TEXT    NOT NULL UNIQUE,
	url           TEXT    NOT NULL,
	interval_ms   INTEGER NOT NULL,
	timeout_ms    INTEGER NOT NULL,
	expect_status INTEGER NOT NULL DEFAULT 0,
	keyword       TEXT    NOT NULL DEFAULT '',
	paused        INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS checks (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	target_id   INTEGER NOT NULL,
	checked_at  INTEGER NOT NULL,
	ok          INTEGER NOT NULL,
	status_code INTEGER NOT NULL,
	latency_ms  INTEGER NOT NULL,
	error       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_checks_target_time ON checks(target_id, checked_at DESC);
CREATE TABLE IF NOT EXISTS incidents (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	target_id   INTEGER NOT NULL,
	started_at  INTEGER NOT NULL,
	ended_at    INTEGER,
	cause       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_incidents_target ON incidents(target_id, started_at DESC);
CREATE TABLE IF NOT EXISTS alerts (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	incident_id INTEGER NOT NULL,
	target_id   INTEGER NOT NULL,
	sent_at     INTEGER NOT NULL,
	channel     TEXT    NOT NULL,
	event       TEXT    NOT NULL,
	ok          INTEGER NOT NULL,
	error       TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_alerts_incident ON alerts(incident_id);
CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS maintenance (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	title      TEXT    NOT NULL,
	starts_at  INTEGER NOT NULL,
	ends_at    INTEGER NOT NULL,
	grp        TEXT    NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_maintenance_time ON maintenance(starts_at, ends_at);
`

// dailySchema holds per-day rollups of checks made outside maintenance. Raw
// checks are pruned after the retention period; rollups are what keep the
// 30-day uptime and the 90-day status history accurate.
const dailySchema = `
CREATE TABLE daily (
	target_id INTEGER NOT NULL,
	day       INTEGER NOT NULL, -- days since the Unix epoch, UTC
	total     INTEGER NOT NULL,
	ok        INTEGER NOT NULL,
	lat_sum   INTEGER NOT NULL, -- sum of latency_ms over successful checks
	PRIMARY KEY (target_id, day)
) WITHOUT ROWID;
INSERT INTO daily (target_id, day, total, ok, lat_sum)
	SELECT target_id, checked_at / 86400000, COUNT(*), COALESCE(SUM(ok), 0),
	       COALESCE(SUM(CASE WHEN ok = 1 THEN latency_ms END), 0)
	FROM checks WHERE maint = 0 GROUP BY target_id, checked_at / 86400000;
`

// migration is one numbered schema step. foreignKeysOff runs it with foreign
// key enforcement disabled, as SQLite's table-rebuild procedure requires.
type migration struct {
	run            func(context.Context, *sql.Tx) error
	foreignKeysOff bool
}

// steps are the numbered schema migrations; step N brings PRAGMA user_version
// to N. Append new steps, never edit existing ones.
var steps = []migration{{run: v1}, {run: v2, foreignKeysOff: true}}

// migrate runs every step above the database's user_version, each in its own
// transaction together with the version bump. path is the database file; when
// the accounts step is about to run on a database that already holds data, a
// backup is written next to it first.
func migrate(ctx context.Context, db *sql.DB, path string) error {
	var cur int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	if cur > len(steps) {
		return fmt.Errorf("database schema version %d is newer than this build supports (%d); use a newer uptime-monitor", cur, len(steps))
	}
	if cur < 2 && len(steps) >= 2 && path != "" {
		has, err := hasData(ctx, db)
		if err != nil {
			return err
		}
		if has {
			existing, err := findBackup(path)
			if err != nil {
				return err
			}
			if existing != "" {
				slog.Info("pre-migration backup already exists", "backup", existing)
			} else {
				dest := path + ".pre-accounts-" + time.Now().Format("20060102-150405") + ".bak"
				if err := Backup(ctx, path, dest); err != nil {
					return fmt.Errorf("pre-migration backup: %w", err)
				}
				slog.Info("backed up database before the accounts migration", "backup", dest)
			}
		}
	}
	for v := cur + 1; v <= len(steps); v++ {
		if err := runStep(ctx, db, v); err != nil {
			return fmt.Errorf("migration %d: %w", v, err)
		}
	}
	return nil
}

// findBackup returns the path of an earlier pre-migration backup of the
// database at path, or "". The directory is listed rather than globbed so
// that characters in path are never read as a pattern. An empty file, left by
// a process killed mid-backup, is removed and does not count.
func findBackup(path string) (string, error) {
	dir, base := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("look for pre-migration backup: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+".pre-accounts-") || !strings.HasSuffix(name, ".bak") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		full := filepath.Join(dir, name)
		if info.Size() == 0 {
			slog.Warn("removing empty pre-migration backup", "backup", full)
			if err := os.Remove(full); err != nil {
				return "", err
			}
			continue
		}
		return full, nil
	}
	return "", nil
}

// hasData reports whether any row exists in targets, settings or maintenance.
// A missing table counts as empty.
func hasData(ctx context.Context, q querier) (bool, error) {
	for _, table := range []string{"targets", "settings", "maintenance"} {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
			return false, err
		}
		if n == 0 {
			continue
		}
		var any int
		err := q.QueryRowContext(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s)`, table)).Scan(&any)
		if err != nil {
			return false, err
		}
		if any == 1 {
			return true, nil
		}
	}
	return false, nil
}

func runStep(ctx context.Context, db *sql.DB, v int) (err error) {
	// Pin the only connection: PRAGMA foreign_keys is per connection and is a
	// no-op inside a transaction, so it is set before BeginTx.
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	m := steps[v-1]
	if m.foreignKeysOff {
		if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
			return err
		}
		defer func() {
			if _, ferr := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`); ferr != nil && err == nil {
				err = ferr
			}
		}()
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := m.run(ctx, tx); err != nil {
		return err
	}
	if m.foreignKeysOff {
		rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
		if err != nil {
			return err
		}
		bad := rows.Next()
		rows.Close()
		if bad {
			return fmt.Errorf("migration %d: foreign key check failed", v)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return tx.Commit()
}

// v1 is the schema of the first numbered release: the base tables plus the
// columns and the daily rollup table added after the very first release. Each
// part checks the current schema first, so it also upgrades older databases.
func v1(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	adds := []struct{ table, col, def string }{
		{"targets", "grp", "TEXT NOT NULL DEFAULT ''"},
		{"targets", "muted", "INTEGER NOT NULL DEFAULT 0"},
		{"incidents", "acked_at", "INTEGER"},
		{"incidents", "note", "TEXT NOT NULL DEFAULT ''"},
		{"checks", "maint", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		has, err := hasColumn(ctx, tx, a.table, a.col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", a.table, a.col, a.def)); err != nil {
			return err
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'daily'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		// Create and backfill in the same transaction so a crash cannot leave an
		// empty rollup table that would never be backfilled. On a large
		// database this takes a while before the server starts listening.
		slog.Info("building daily uptime rollups from existing checks (one-time upgrade)")
		start := time.Now()
		if _, err := tx.ExecContext(ctx, dailySchema); err != nil {
			return fmt.Errorf("create daily rollups: %w", err)
		}
		slog.Info("daily uptime rollups built", "took", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func hasColumn(ctx context.Context, q querier, table, col string) (bool, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

const accountsSchema = `
CREATE TABLE users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	email         TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	is_operator   INTEGER NOT NULL DEFAULT 0,
	verified_at   INTEGER,
	disabled_at   INTEGER,
	created_at    INTEGER NOT NULL,
	last_seen_at  INTEGER
);
CREATE TABLE workspaces (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT    NOT NULL,
	slug         TEXT    NOT NULL UNIQUE,
	suspended_at INTEGER,
	created_at   INTEGER NOT NULL
);
CREATE TABLE memberships (
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	role         TEXT    NOT NULL CHECK (role IN ('owner','editor','viewer')),
	created_at   INTEGER NOT NULL,
	PRIMARY KEY (user_id, workspace_id)
);
CREATE TABLE sessions (
	token_hash   TEXT    PRIMARY KEY,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at   INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL,
	expires_at   INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);
CREATE TABLE tokens (
	token_hash TEXT    PRIMARY KEY,
	kind       TEXT    NOT NULL CHECK (kind IN ('setup','invite','verify','reset')),
	user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE,
	email      TEXT,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER
);
CREATE INDEX idx_tokens_user ON tokens(user_id, kind);
`

const targetsRebuild = `
CREATE TABLE targets_new (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_id  INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	name          TEXT    NOT NULL,
	url           TEXT    NOT NULL,
	interval_ms   INTEGER NOT NULL,
	timeout_ms    INTEGER NOT NULL,
	expect_status INTEGER NOT NULL DEFAULT 0,
	keyword       TEXT    NOT NULL DEFAULT '',
	paused        INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	grp           TEXT    NOT NULL DEFAULT '',
	muted         INTEGER NOT NULL DEFAULT 0,
	UNIQUE (workspace_id, name)
);
INSERT INTO targets_new (id, workspace_id, name, url, interval_ms, timeout_ms, expect_status, keyword, paused, created_at, grp, muted)
	SELECT id, 1, name, url, interval_ms, timeout_ms, expect_status, keyword, paused, created_at, grp, muted FROM targets;
`

const maintenanceRebuild = `
CREATE TABLE maintenance_new (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	title        TEXT    NOT NULL,
	starts_at    INTEGER NOT NULL,
	ends_at      INTEGER NOT NULL,
	grp          TEXT    NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL
);
INSERT INTO maintenance_new (id, workspace_id, title, starts_at, ends_at, grp, created_at)
	SELECT id, 1, title, starts_at, ends_at, grp, created_at FROM maintenance;
`

const settingsRebuild = `
CREATE TABLE settings_new (
	workspace_id INTEGER NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
	key          TEXT    NOT NULL,
	value        TEXT    NOT NULL,
	PRIMARY KEY (workspace_id, key)
);
INSERT INTO settings_new (workspace_id, key, value) SELECT 1, key, value FROM settings;
DROP TABLE settings;
ALTER TABLE settings_new RENAME TO settings;
`

// v2 adds the accounts tables and makes targets, maintenance and settings
// belong to a workspace. Every existing row moves into workspace 1. It must run
// with foreign keys off (see runStep) because it drops and renames tables.
func v2(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, accountsSchema); err != nil {
		return fmt.Errorf("create accounts tables: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces (id, name, slug, created_at) VALUES (1, 'Default', 'default', ?)`, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("create default workspace: %w", err)
	}
	if err := rebuild(ctx, tx, "targets", targetsRebuild, ""); err != nil {
		return err
	}
	if err := rebuild(ctx, tx, "maintenance", maintenanceRebuild,
		`CREATE INDEX idx_maintenance_time ON maintenance(starts_at, ends_at);
		 CREATE INDEX idx_maintenance_ws ON maintenance(workspace_id, starts_at);`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, settingsRebuild); err != nil {
		return fmt.Errorf("rebuild settings: %w", err)
	}
	return nil
}

// rebuild replaces table with its <table>_new definition (created and filled by
// create), then restores the AUTOINCREMENT counter so deleted ids are never
// reused: dropping a table deletes its sqlite_sequence row.
func rebuild(ctx context.Context, tx *sql.Tx, table, create, indexes string) error {
	var oldSeq int64
	err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&oldSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, q := range []string{create, `DROP TABLE ` + table, `ALTER TABLE ` + table + `_new RENAME TO ` + table, indexes} {
		if q == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild %s: %w", table, err)
		}
	}
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM `+table).Scan(&maxID); err != nil {
		return err
	}
	seq := max(oldSeq, maxID)
	if seq == 0 {
		return nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = ?`, seq, table)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, table, seq)
	}
	return err
}
