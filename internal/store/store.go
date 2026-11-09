// Package store persists targets, checks, incidents and alerts in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Target is a monitored URL.
type Target struct {
	ID           int64
	Name         string
	URL          string
	Interval     time.Duration
	Timeout      time.Duration
	ExpectStatus int
	Keyword      string
	Group        string
	Paused       bool
	Muted        bool // record incidents but send no alerts
	CreatedAt    time.Time
}

// Check is one probe of a target.
type Check struct {
	ID         int64
	TargetID   int64
	CheckedAt  time.Time
	OK         bool
	StatusCode int
	LatencyMS  int64
	Error      string
	// Maintenance marks a check made inside a maintenance window. Such
	// checks are kept for the latency history but excluded from uptime.
	Maintenance bool
}

// Maintenance is a planned window during which downtime is expected. It
// applies to every target, or only to those in Group when it is set.
type Maintenance struct {
	ID        int64
	Title     string
	StartsAt  time.Time
	EndsAt    time.Time
	Group     string // "" means all targets
	CreatedAt time.Time
}

// Active reports whether the window covers the instant at.
func (m Maintenance) Active(at time.Time) bool {
	return !at.Before(m.StartsAt) && at.Before(m.EndsAt)
}

// Upcoming reports whether the window has not started yet.
func (m Maintenance) Upcoming(at time.Time) bool { return at.Before(m.StartsAt) }

// Covers reports whether the window applies to a target in the given group.
func (m Maintenance) Covers(group string) bool { return m.Group == "" || m.Group == group }

// Incident is a contiguous period of downtime.
type Incident struct {
	ID         int64
	TargetID   int64
	TargetName string
	StartedAt  time.Time
	EndedAt    *time.Time
	Cause      string
	Channels   string // comma-separated channels that were alerted successfully
	AckedAt    *time.Time
	Note       string
}

// Acknowledged reports whether someone has acknowledged the incident.
func (i Incident) Acknowledged() bool { return i.AckedAt != nil }

// Duration returns how long the incident lasted, or has lasted so far.
func (i Incident) Duration(now time.Time) time.Duration {
	if i.EndedAt != nil {
		return i.EndedAt.Sub(i.StartedAt)
	}
	return now.Sub(i.StartedAt)
}

// Ongoing reports whether the incident has not ended.
func (i Incident) Ongoing() bool { return i.EndedAt == nil }

// Alert is one delivery attempt of a notification.
type Alert struct {
	ID         int64
	IncidentID int64
	TargetID   int64
	SentAt     time.Time
	Channel    string
	Event      string // "down" or "up"
	OK         bool
	Error      string
}

// Stats summarises checks over a window.
type Stats struct {
	Total        int
	OKCount      int
	AvgLatencyMS float64
}

// UptimePercent returns the share of successful checks, or 0 with no data.
func (s Stats) UptimePercent() float64 {
	if s.Total == 0 {
		return 0
	}
	return 100 * float64(s.OKCount) / float64(s.Total)
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

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

// Open opens or creates the database at path and ensures the schema exists.
func Open(path string) (*Store, error) {
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

// migrate adds columns introduced after the first release. ALTER TABLE ADD
// COLUMN is idempotent here because we check the column list first.
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

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- targets ----

const targetCols = `id, name, url, interval_ms, timeout_ms, expect_status, keyword, grp, paused, muted, created_at`

func scanTarget(sc scanner) (Target, error) {
	var t Target
	var interval, timeout, created int64
	var paused, muted int
	if err := sc.Scan(&t.ID, &t.Name, &t.URL, &interval, &timeout, &t.ExpectStatus, &t.Keyword, &t.Group, &paused, &muted, &created); err != nil {
		return Target{}, err
	}
	t.Interval = time.Duration(interval) * time.Millisecond
	t.Timeout = time.Duration(timeout) * time.Millisecond
	t.Paused = paused == 1
	t.Muted = muted == 1
	t.CreatedAt = time.UnixMilli(created).UTC()
	return t, nil
}

// ListTargets returns all targets ordered by name.
func (s *Store) ListTargets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+targetCols+` FROM targets ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTarget returns one target by id.
func (s *Store) GetTarget(ctx context.Context, id int64) (Target, error) {
	t, err := scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	return t, err
}

// GetTargetByName returns one target by name.
func (s *Store) GetTargetByName(ctx context.Context, name string) (Target, error) {
	t, err := scanTarget(s.db.QueryRowContext(ctx, `SELECT `+targetCols+` FROM targets WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, ErrNotFound
	}
	return t, err
}

// CreateTarget inserts a target and returns it with its id.
func (s *Store) CreateTarget(ctx context.Context, t Target, now time.Time) (Target, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO targets (name, url, interval_ms, timeout_ms, expect_status, keyword, grp, paused, muted, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.Name, t.URL, t.Interval.Milliseconds(), t.Timeout.Milliseconds(), t.ExpectStatus, t.Keyword, t.Group, boolToInt(t.Paused), boolToInt(t.Muted), now.UnixMilli())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Target{}, fmt.Errorf("a target named %q already exists", t.Name)
		}
		return Target{}, err
	}
	t.ID, _ = res.LastInsertId()
	t.CreatedAt = now.UTC()
	return t, nil
}

// UpdateTarget rewrites the editable fields of a target.
func (s *Store) UpdateTarget(ctx context.Context, t Target) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE targets SET name = ?, url = ?, interval_ms = ?, timeout_ms = ?, expect_status = ?, keyword = ?, grp = ?, paused = ?, muted = ?
		 WHERE id = ?`,
		t.Name, t.URL, t.Interval.Milliseconds(), t.Timeout.Milliseconds(), t.ExpectStatus, t.Keyword, t.Group, boolToInt(t.Paused), boolToInt(t.Muted), t.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("a target named %q already exists", t.Name)
		}
		return err
	}
	return requireRow(res)
}

// SetPaused pauses or resumes a target.
func (s *Store) SetPaused(ctx context.Context, id int64, paused bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET paused = ? WHERE id = ?`, boolToInt(paused), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// SetMuted mutes or unmutes alerts for a target.
func (s *Store) SetMuted(ctx context.Context, id int64, muted bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE targets SET muted = ? WHERE id = ?`, boolToInt(muted), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeleteTarget removes a target and all of its history.
func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM alerts WHERE target_id = ?`,
		`DELETE FROM incidents WHERE target_id = ?`,
		`DELETE FROM checks WHERE target_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM targets WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := requireRow(res); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- checks ----

const checkCols = `id, target_id, checked_at, ok, status_code, latency_ms, error, maint`

func scanCheck(sc scanner) (Check, error) {
	var c Check
	var checkedAt int64
	var ok, maint int
	if err := sc.Scan(&c.ID, &c.TargetID, &checkedAt, &ok, &c.StatusCode, &c.LatencyMS, &c.Error, &maint); err != nil {
		return Check{}, err
	}
	c.CheckedAt = time.UnixMilli(checkedAt).UTC()
	c.OK = ok == 1
	c.Maintenance = maint == 1
	return c, nil
}

// InsertCheck records a check.
func (s *Store) InsertCheck(ctx context.Context, c Check) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO checks (target_id, checked_at, ok, status_code, latency_ms, error, maint) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.TargetID, c.CheckedAt.UnixMilli(), boolToInt(c.OK), c.StatusCode, c.LatencyMS, c.Error, boolToInt(c.Maintenance))
	return err
}

// LatestCheck returns the most recent check for a target, or ErrNotFound.
func (s *Store) LatestCheck(ctx context.Context, targetID int64) (Check, error) {
	c, err := scanCheck(s.db.QueryRowContext(ctx,
		`SELECT `+checkCols+` FROM checks WHERE target_id = ? ORDER BY checked_at DESC, id DESC LIMIT 1`, targetID))
	if errors.Is(err, sql.ErrNoRows) {
		return Check{}, ErrNotFound
	}
	return c, err
}

// RecentChecks returns up to limit checks, newest first.
func (s *Store) RecentChecks(ctx context.Context, targetID int64, limit int) ([]Check, error) {
	return s.queryChecks(ctx,
		`SELECT `+checkCols+` FROM checks WHERE target_id = ? ORDER BY checked_at DESC, id DESC LIMIT ?`, targetID, limit)
}

// ChecksSince returns checks at or after since, oldest first.
func (s *Store) ChecksSince(ctx context.Context, targetID int64, since time.Time) ([]Check, error) {
	return s.queryChecks(ctx,
		`SELECT `+checkCols+` FROM checks WHERE target_id = ? AND checked_at >= ? ORDER BY checked_at ASC, id ASC`,
		targetID, since.UnixMilli())
}

func (s *Store) queryChecks(ctx context.Context, q string, args ...any) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Check
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Stats aggregates checks for a target since the given time. A targetID of
// 0 aggregates across all targets. Checks made during maintenance windows
// are left out, as are they from every other uptime aggregate below.
func (s *Store) Stats(ctx context.Context, targetID int64, since time.Time) (Stats, error) {
	var st Stats
	var avg sql.NullFloat64
	q := `SELECT COUNT(*), COALESCE(SUM(ok), 0), AVG(CASE WHEN ok = 1 THEN latency_ms END)
	      FROM checks WHERE maint = 0 AND checked_at >= ?`
	args := []any{since.UnixMilli()}
	if targetID != 0 {
		q += ` AND target_id = ?`
		args = append(args, targetID)
	}
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&st.Total, &st.OKCount, &avg); err != nil {
		return Stats{}, err
	}
	if avg.Valid {
		st.AvgLatencyMS = avg.Float64
	}
	return st, nil
}

// DailyUptime returns per-day uptime percentages for the last days days,
// oldest first. Days with no checks have Total 0.
func (s *Store) DailyUptime(ctx context.Context, targetID int64, now time.Time, days int) ([]Stats, error) {
	start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	rows, err := s.db.QueryContext(ctx,
		`SELECT (checked_at / 86400000) AS day, COUNT(*), COALESCE(SUM(ok), 0)
		 FROM checks WHERE maint = 0 AND target_id = ? AND checked_at >= ? GROUP BY day`, targetID, start.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[int64]Stats{}
	for rows.Next() {
		var day int64
		var st Stats
		if err := rows.Scan(&day, &st.Total, &st.OKCount); err != nil {
			return nil, err
		}
		byDay[day] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Stats, days)
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i).UnixMilli() / 86400000
		out[i] = byDay[day]
	}
	return out, nil
}

// Latencies returns the latencies of successful checks since the given
// time, sorted ascending. A targetID of 0 covers all targets.
func (s *Store) Latencies(ctx context.Context, targetID int64, since time.Time) ([]int64, error) {
	q := `SELECT latency_ms FROM checks WHERE maint = 0 AND ok = 1 AND checked_at >= ?`
	args := []any{since.UnixMilli()}
	if targetID != 0 {
		q += ` AND target_id = ?`
		args = append(args, targetID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY latency_ms ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Percentile returns the p-th percentile (0-100) of sorted values, or 0.
func Percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p / 100)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// HourlyUptime returns per-hour stats for the last hours hours ending at
// now, oldest first.
func (s *Store) HourlyUptime(ctx context.Context, targetID int64, now time.Time, hours int) ([]Stats, error) {
	start := now.UTC().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)
	rows, err := s.db.QueryContext(ctx,
		`SELECT (checked_at / 3600000) AS hr, COUNT(*), COALESCE(SUM(ok), 0), AVG(CASE WHEN ok = 1 THEN latency_ms END)
		 FROM checks WHERE maint = 0 AND target_id = ? AND checked_at >= ? GROUP BY hr`, targetID, start.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byHour := map[int64]Stats{}
	for rows.Next() {
		var hr int64
		var st Stats
		var avg sql.NullFloat64
		if err := rows.Scan(&hr, &st.Total, &st.OKCount, &avg); err != nil {
			return nil, err
		}
		st.AvgLatencyMS = avg.Float64
		byHour[hr] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Stats, hours)
	for i := 0; i < hours; i++ {
		out[i] = byHour[start.Add(time.Duration(i)*time.Hour).UnixMilli()/3600000]
	}
	return out, nil
}

// Prune deletes checks older than before and returns how many were removed.
func (s *Store) Prune(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE checked_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- incidents ----

const incidentQuery = `
SELECT i.id, i.target_id, t.name, i.started_at, i.ended_at, i.cause,
       COALESCE((SELECT GROUP_CONCAT(DISTINCT a.channel) FROM alerts a WHERE a.incident_id = i.id AND a.ok = 1), ''),
       i.acked_at, i.note
FROM incidents i JOIN targets t ON t.id = i.target_id`

func scanIncident(sc scanner) (Incident, error) {
	var in Incident
	var started int64
	var ended, acked sql.NullInt64
	if err := sc.Scan(&in.ID, &in.TargetID, &in.TargetName, &started, &ended, &in.Cause, &in.Channels, &acked, &in.Note); err != nil {
		return Incident{}, err
	}
	in.StartedAt = time.UnixMilli(started).UTC()
	if ended.Valid {
		e := time.UnixMilli(ended.Int64).UTC()
		in.EndedAt = &e
	}
	if acked.Valid {
		a := time.UnixMilli(acked.Int64).UTC()
		in.AckedAt = &a
	}
	return in, nil
}

// GetIncident returns one incident by id.
func (s *Store) GetIncident(ctx context.Context, id int64) (Incident, error) {
	in, err := scanIncident(s.db.QueryRowContext(ctx, incidentQuery+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, ErrNotFound
	}
	return in, err
}

// AcknowledgeIncident marks an incident as seen by a human.
func (s *Store) AcknowledgeIncident(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE incidents SET acked_at = ? WHERE id = ? AND acked_at IS NULL`, at.UnixMilli(), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// SetIncidentNote stores a free-text note on an incident.
func (s *Store) SetIncidentNote(ctx context.Context, id int64, note string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE incidents SET note = ? WHERE id = ?`, note, id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// CountUnacknowledged counts ongoing incidents nobody has acknowledged.
func (s *Store) CountUnacknowledged(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incidents WHERE ended_at IS NULL AND acked_at IS NULL`).Scan(&n)
	return n, err
}

// OpenIncident starts a new incident and returns its id.
func (s *Store) OpenIncident(ctx context.Context, targetID int64, startedAt time.Time, cause string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO incidents (target_id, started_at, cause) VALUES (?, ?, ?)`, targetID, startedAt.UnixMilli(), cause)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CloseIncident marks an incident as ended.
func (s *Store) CloseIncident(ctx context.Context, id int64, endedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE incidents SET ended_at = ? WHERE id = ? AND ended_at IS NULL`, endedAt.UnixMilli(), id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// OngoingIncident returns the open incident for a target, or ErrNotFound.
func (s *Store) OngoingIncident(ctx context.Context, targetID int64) (Incident, error) {
	in, err := scanIncident(s.db.QueryRowContext(ctx,
		incidentQuery+` WHERE i.target_id = ? AND i.ended_at IS NULL ORDER BY i.started_at DESC LIMIT 1`, targetID))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, ErrNotFound
	}
	return in, err
}

// IncidentFilter narrows ListIncidents.
type IncidentFilter struct {
	TargetID    int64 // 0 for all
	OngoingOnly bool
	Since       time.Time // zero for all time
	Limit       int       // 0 for default 200
}

// ListIncidents returns incidents newest first.
func (s *Store) ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error) {
	q := incidentQuery + ` WHERE 1=1`
	var args []any
	if f.TargetID != 0 {
		q += ` AND i.target_id = ?`
		args = append(args, f.TargetID)
	}
	if f.OngoingOnly {
		q += ` AND i.ended_at IS NULL`
	}
	if !f.Since.IsZero() {
		q += ` AND i.started_at >= ?`
		args = append(args, f.Since.UnixMilli())
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q += ` ORDER BY i.started_at DESC, i.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incident
	for rows.Next() {
		in, err := scanIncident(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// CountIncidents counts incidents started since the given time.
func (s *Store) CountIncidents(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incidents WHERE started_at >= ?`, since.UnixMilli()).Scan(&n)
	return n, err
}

// ---- alerts ----

// InsertAlert records a delivery attempt.
func (s *Store) InsertAlert(ctx context.Context, a Alert) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO alerts (incident_id, target_id, sent_at, channel, event, ok, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.IncidentID, a.TargetID, a.SentAt.UnixMilli(), a.Channel, a.Event, boolToInt(a.OK), a.Error)
	return err
}

// LastAlerts returns the most recent delivery attempt per channel.
func (s *Store) LastAlerts(ctx context.Context) (map[string]Alert, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, a.incident_id, a.target_id, a.sent_at, a.channel, a.event, a.ok, a.error
		 FROM alerts a JOIN (SELECT channel, MAX(id) AS id FROM alerts GROUP BY channel) m ON m.id = a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Alert{}
	for rows.Next() {
		var a Alert
		var sent int64
		var ok int
		if err := rows.Scan(&a.ID, &a.IncidentID, &a.TargetID, &sent, &a.Channel, &a.Event, &ok, &a.Error); err != nil {
			return nil, err
		}
		a.SentAt = time.UnixMilli(sent).UTC()
		a.OK = ok == 1
		out[a.Channel] = a
	}
	return out, rows.Err()
}

// CountAlerts counts successful deliveries since the given time.
func (s *Store) CountAlerts(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE ok = 1 AND sent_at >= ?`, since.UnixMilli()).Scan(&n)
	return n, err
}

// ---- maintenance ----

const maintCols = `id, title, starts_at, ends_at, grp, created_at`

func scanMaintenance(sc scanner) (Maintenance, error) {
	var m Maintenance
	var starts, ends, created int64
	if err := sc.Scan(&m.ID, &m.Title, &starts, &ends, &m.Group, &created); err != nil {
		return Maintenance{}, err
	}
	m.StartsAt = time.UnixMilli(starts).UTC()
	m.EndsAt = time.UnixMilli(ends).UTC()
	m.CreatedAt = time.UnixMilli(created).UTC()
	return m, nil
}

// CreateMaintenance schedules a window and returns it with its id.
func (s *Store) CreateMaintenance(ctx context.Context, m Maintenance, now time.Time) (Maintenance, error) {
	if !m.EndsAt.After(m.StartsAt) {
		return Maintenance{}, errors.New("maintenance must end after it starts")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO maintenance (title, starts_at, ends_at, grp, created_at) VALUES (?, ?, ?, ?, ?)`,
		m.Title, m.StartsAt.UnixMilli(), m.EndsAt.UnixMilli(), m.Group, now.UnixMilli())
	if err != nil {
		return Maintenance{}, err
	}
	m.ID, _ = res.LastInsertId()
	m.StartsAt, m.EndsAt, m.CreatedAt = m.StartsAt.UTC(), m.EndsAt.UTC(), now.UTC()
	return m, nil
}

// GetMaintenance returns one window by id.
func (s *Store) GetMaintenance(ctx context.Context, id int64) (Maintenance, error) {
	m, err := scanMaintenance(s.db.QueryRowContext(ctx, `SELECT `+maintCols+` FROM maintenance WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Maintenance{}, ErrNotFound
	}
	return m, err
}

// EndMaintenance cuts an active window short at the given time. Windows
// that have not started are deleted instead; ended ones are left alone.
func (s *Store) EndMaintenance(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE maintenance SET ends_at = ? WHERE id = ? AND starts_at <= ? AND ends_at > ?`,
		at.UnixMilli(), id, at.UnixMilli(), at.UnixMilli())
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeleteMaintenance removes a window.
func (s *Store) DeleteMaintenance(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM maintenance WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// ListMaintenance returns windows that end after since, soonest first.
// Pass the zero time for the full history.
func (s *Store) ListMaintenance(ctx context.Context, since time.Time, limit int) ([]Maintenance, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE ends_at > ? ORDER BY starts_at ASC, id ASC LIMIT ?`, since.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Maintenance
	for rows.Next() {
		m, err := scanMaintenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActiveMaintenance returns the windows covering the instant at.
func (s *Store) ActiveMaintenance(ctx context.Context, at time.Time) ([]Maintenance, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE starts_at <= ? AND ends_at > ? ORDER BY starts_at ASC`, at.UnixMilli(), at.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Maintenance
	for rows.Next() {
		m, err := scanMaintenance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// InMaintenance reports whether a target in group is covered by an active
// window at the given time, returning that window.
func (s *Store) InMaintenance(ctx context.Context, group string, at time.Time) (Maintenance, bool, error) {
	m, err := scanMaintenance(s.db.QueryRowContext(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE starts_at <= ? AND ends_at > ? AND (grp = '' OR grp = ?)
		 ORDER BY starts_at ASC LIMIT 1`, at.UnixMilli(), at.UnixMilli(), group))
	if errors.Is(err, sql.ErrNoRows) {
		return Maintenance{}, false, nil
	}
	if err != nil {
		return Maintenance{}, false, err
	}
	return m, true, nil
}

// ---- settings ----

// GetSetting returns a setting value, or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
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
