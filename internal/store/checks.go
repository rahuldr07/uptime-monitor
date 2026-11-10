package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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

// InsertCheck records a check and, unless it was made during maintenance,
// adds it to that day's rollup.
func (s *Store) InsertCheck(ctx context.Context, c Check) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO checks (target_id, checked_at, ok, status_code, latency_ms, error, maint) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.TargetID, c.CheckedAt.UnixMilli(), boolToInt(c.OK), c.StatusCode, c.LatencyMS, c.Error, boolToInt(c.Maintenance)); err != nil {
		return err
	}
	if !c.Maintenance {
		var lat int64
		if c.OK {
			lat = c.LatencyMS
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO daily (target_id, day, total, ok, lat_sum) VALUES (?, ?, 1, ?, ?)
			 ON CONFLICT(target_id, day) DO UPDATE SET total = total + 1, ok = ok + excluded.ok, lat_sum = lat_sum + excluded.lat_sum`,
			c.TargetID, c.CheckedAt.UnixMilli()/dayMS, boolToInt(c.OK), lat); err != nil {
			return err
		}
	}
	return tx.Commit()
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

// DailyUptime returns per-day (UTC) stats for the last days days, oldest
// first, from the rollups. Days with no checks have Total 0.
func (s *Store) DailyUptime(ctx context.Context, targetID int64, now time.Time, days int) ([]Stats, error) {
	start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	rows, err := s.db.QueryContext(ctx,
		`SELECT day, total, ok, lat_sum FROM daily WHERE target_id = ? AND day >= ?`, targetID, start.UnixMilli()/dayMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDay := map[int64]Stats{}
	for rows.Next() {
		var day, latSum int64
		var st Stats
		if err := rows.Scan(&day, &st.Total, &st.OKCount, &latSum); err != nil {
			return nil, err
		}
		if st.OKCount > 0 {
			st.AvgLatencyMS = float64(latSum) / float64(st.OKCount)
		}
		byDay[day] = st
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Stats, days)
	for i := 0; i < days; i++ {
		out[i] = byDay[start.AddDate(0, 0, i).UnixMilli()/dayMS]
	}
	return out, nil
}

// RollupStats aggregates the daily rollups of whole UTC days from the one
// containing since up to now. It covers periods longer than the raw check
// retention; the first day is counted in full, so the window can be up to a
// day longer than asked. A targetID of 0 aggregates across all targets.
func (s *Store) RollupStats(ctx context.Context, targetID int64, since time.Time) (Stats, error) {
	q := `SELECT COALESCE(SUM(total), 0), COALESCE(SUM(ok), 0), COALESCE(SUM(lat_sum), 0) FROM daily WHERE day >= ?`
	args := []any{since.UnixMilli() / dayMS}
	if targetID != 0 {
		q += ` AND target_id = ?`
		args = append(args, targetID)
	}
	var st Stats
	var latSum int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&st.Total, &st.OKCount, &latSum); err != nil {
		return Stats{}, err
	}
	if st.OKCount > 0 {
		st.AvgLatencyMS = float64(latSum) / float64(st.OKCount)
	}
	return st, nil
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

// PruneRollups deletes daily rollups for days that ended before before.
func (s *Store) PruneRollups(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM daily WHERE day < ?`, before.UnixMilli()/dayMS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
