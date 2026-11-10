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

// RecentChecks returns up to limit of the workspace's checks, newest first.
// A targetID of 0 covers all of the workspace's targets.
func (w *WS) RecentChecks(ctx context.Context, targetID int64, limit int) ([]Check, error) {
	cond, args := w.scope(targetID)
	return w.queryChecks(ctx,
		`SELECT `+checkCols+` FROM checks WHERE `+cond+` ORDER BY checked_at DESC, id DESC LIMIT ?`, append(args, limit)...)
}

// ChecksSince returns the workspace's checks at or after since, oldest first.
// A targetID of 0 covers all of the workspace's targets.
func (w *WS) ChecksSince(ctx context.Context, targetID int64, since time.Time) ([]Check, error) {
	cond, args := w.scope(targetID)
	return w.queryChecks(ctx,
		`SELECT `+checkCols+` FROM checks WHERE `+cond+` AND checked_at >= ? ORDER BY checked_at ASC, id ASC`,
		append(args, since.UnixMilli())...)
}

func (w *WS) queryChecks(ctx context.Context, q string, args ...any) ([]Check, error) {
	rows, err := w.db.QueryContext(ctx, q, args...)
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

// Stats aggregates the workspace's checks since the given time. A targetID of
// 0 aggregates across all of its targets. Maintenance checks are left out.
func (w *WS) Stats(ctx context.Context, targetID int64, since time.Time) (Stats, error) {
	cond, args := w.scope(targetID)
	var st Stats
	var avg sql.NullFloat64
	err := w.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(ok), 0), AVG(CASE WHEN ok = 1 THEN latency_ms END)
		 FROM checks WHERE maint = 0 AND checked_at >= ? AND `+cond, append([]any{since.UnixMilli()}, args...)...).
		Scan(&st.Total, &st.OKCount, &avg)
	if err != nil {
		return Stats{}, err
	}
	if avg.Valid {
		st.AvgLatencyMS = avg.Float64
	}
	return st, nil
}

// DailyUptime returns per-day (UTC) stats for the last days days, oldest
// first, from the rollups. Days with no checks have Total 0. A targetID of 0
// sums all of the workspace's targets per day.
func (w *WS) DailyUptime(ctx context.Context, targetID int64, now time.Time, days int) ([]Stats, error) {
	start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	cond, args := w.scope(targetID)
	rows, err := w.db.QueryContext(ctx,
		`SELECT day, SUM(total), SUM(ok), SUM(lat_sum) FROM daily WHERE day >= ? AND `+cond+` GROUP BY day`,
		append([]any{start.UnixMilli() / dayMS}, args...)...)
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

// RollupStats aggregates the workspace's daily rollups of whole UTC days from
// the one containing since up to now. It covers periods longer than the raw
// check retention; the first day is counted in full, so the window can be up
// to a day longer than asked. A targetID of 0 aggregates across all of the
// workspace's targets.
func (w *WS) RollupStats(ctx context.Context, targetID int64, since time.Time) (Stats, error) {
	cond, args := w.scope(targetID)
	var st Stats
	var latSum int64
	err := w.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total), 0), COALESCE(SUM(ok), 0), COALESCE(SUM(lat_sum), 0) FROM daily WHERE day >= ? AND `+cond,
		append([]any{since.UnixMilli() / dayMS}, args...)...).Scan(&st.Total, &st.OKCount, &latSum)
	if err != nil {
		return Stats{}, err
	}
	if st.OKCount > 0 {
		st.AvgLatencyMS = float64(latSum) / float64(st.OKCount)
	}
	return st, nil
}

// Latencies returns the latencies of the workspace's successful checks since
// the given time, sorted ascending. A targetID of 0 covers all of its targets.
func (w *WS) Latencies(ctx context.Context, targetID int64, since time.Time) ([]int64, error) {
	cond, args := w.scope(targetID)
	rows, err := w.db.QueryContext(ctx,
		`SELECT latency_ms FROM checks WHERE maint = 0 AND ok = 1 AND checked_at >= ? AND `+cond+` ORDER BY latency_ms ASC`,
		append([]any{since.UnixMilli()}, args...)...)
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
// now, oldest first. A targetID of 0 sums all of the workspace's targets.
func (w *WS) HourlyUptime(ctx context.Context, targetID int64, now time.Time, hours int) ([]Stats, error) {
	start := now.UTC().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)
	cond, args := w.scope(targetID)
	rows, err := w.db.QueryContext(ctx,
		`SELECT (checked_at / 3600000) AS hr, COUNT(*), COALESCE(SUM(ok), 0), AVG(CASE WHEN ok = 1 THEN latency_ms END)
		 FROM checks WHERE maint = 0 AND checked_at >= ? AND `+cond+` GROUP BY hr`,
		append([]any{start.UnixMilli()}, args...)...)
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
