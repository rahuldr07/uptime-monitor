package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

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
		`INSERT INTO targets (workspace_id, name, url, interval_ms, timeout_ms, expect_status, keyword, grp, paused, muted, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		legacyWorkspaceID, t.Name, t.URL, t.Interval.Milliseconds(), t.Timeout.Milliseconds(), t.ExpectStatus, t.Keyword, t.Group, boolToInt(t.Paused), boolToInt(t.Muted), now.UnixMilli())
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
		`DELETE FROM daily WHERE target_id = ?`,
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
