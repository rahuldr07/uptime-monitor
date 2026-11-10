package store

import (
	"database/sql"
	"fmt"
	"log/slog"
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

// migrate adds columns and tables introduced after the first release. Each
// step checks the current schema first, so it is safe to run on every start.
func migrate(db *sql.DB) error {
	adds := []struct{ table, col, def string }{
		{"targets", "grp", "TEXT NOT NULL DEFAULT ''"},
		{"targets", "muted", "INTEGER NOT NULL DEFAULT 0"},
		{"incidents", "acked_at", "INTEGER"},
		{"incidents", "note", "TEXT NOT NULL DEFAULT ''"},
		{"checks", "maint", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range adds {
		has, err := hasColumn(db, a.table, a.col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", a.table, a.col, a.def)); err != nil {
			return err
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'daily'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		// Create and backfill in one transaction so a crash cannot leave an
		// empty rollup table that would never be backfilled. On a large
		// database this takes a while before the server starts listening.
		slog.Info("building daily uptime rollups from existing checks (one-time upgrade)")
		start := time.Now()
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.Exec(dailySchema); err != nil {
			return fmt.Errorf("create daily rollups: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		slog.Info("daily uptime rollups built", "took", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
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
