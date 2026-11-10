package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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

// CreateMaintenance schedules a window in the workspace and returns it.
func (w *WS) CreateMaintenance(ctx context.Context, m Maintenance, now time.Time) (Maintenance, error) {
	if !m.EndsAt.After(m.StartsAt) {
		return Maintenance{}, errors.New("maintenance must end after it starts")
	}
	res, err := w.db.ExecContext(ctx,
		`INSERT INTO maintenance (workspace_id, title, starts_at, ends_at, grp, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		w.id, m.Title, m.StartsAt.UnixMilli(), m.EndsAt.UnixMilli(), m.Group, now.UnixMilli())
	if err != nil {
		return Maintenance{}, err
	}
	m.ID, _ = res.LastInsertId()
	m.StartsAt, m.EndsAt, m.CreatedAt = m.StartsAt.UTC(), m.EndsAt.UTC(), now.UTC()
	return m, nil
}

// GetMaintenance returns one of the workspace's windows by id.
func (w *WS) GetMaintenance(ctx context.Context, id int64) (Maintenance, error) {
	m, err := scanMaintenance(w.db.QueryRowContext(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE id = ? AND workspace_id = ?`, id, w.id))
	if errors.Is(err, sql.ErrNoRows) {
		return Maintenance{}, ErrNotFound
	}
	return m, err
}

// EndMaintenance cuts one of the workspace's active windows short.
func (w *WS) EndMaintenance(ctx context.Context, id int64, at time.Time) error {
	res, err := w.db.ExecContext(ctx,
		`UPDATE maintenance SET ends_at = ? WHERE id = ? AND workspace_id = ? AND starts_at <= ? AND ends_at > ?`,
		at.UnixMilli(), id, w.id, at.UnixMilli(), at.UnixMilli())
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeleteMaintenance removes one of the workspace's windows.
func (w *WS) DeleteMaintenance(ctx context.Context, id int64) error {
	res, err := w.db.ExecContext(ctx, `DELETE FROM maintenance WHERE id = ? AND workspace_id = ?`, id, w.id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// ListMaintenance returns the workspace's windows that end after since.
func (w *WS) ListMaintenance(ctx context.Context, since time.Time, limit int) ([]Maintenance, error) {
	if limit <= 0 {
		limit = 200
	}
	return w.queryMaintenance(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE workspace_id = ? AND ends_at > ? ORDER BY starts_at ASC, id ASC LIMIT ?`,
		w.id, since.UnixMilli(), limit)
}

// ActiveMaintenance returns the workspace's windows covering the instant at.
func (w *WS) ActiveMaintenance(ctx context.Context, at time.Time) ([]Maintenance, error) {
	return w.queryMaintenance(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE workspace_id = ? AND starts_at <= ? AND ends_at > ? ORDER BY starts_at ASC`,
		w.id, at.UnixMilli(), at.UnixMilli())
}

func (w *WS) queryMaintenance(ctx context.Context, q string, args ...any) ([]Maintenance, error) {
	rows, err := w.db.QueryContext(ctx, q, args...)
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

// InMaintenance reports whether a target in group is covered by one of the
// workspace's active windows at the given time, returning that window.
func (w *WS) InMaintenance(ctx context.Context, group string, at time.Time) (Maintenance, bool, error) {
	m, err := scanMaintenance(w.db.QueryRowContext(ctx,
		`SELECT `+maintCols+` FROM maintenance WHERE workspace_id = ? AND starts_at <= ? AND ends_at > ? AND (grp = '' OR grp = ?)
		 ORDER BY starts_at ASC LIMIT 1`, w.id, at.UnixMilli(), at.UnixMilli(), group))
	if errors.Is(err, sql.ErrNoRows) {
		return Maintenance{}, false, nil
	}
	if err != nil {
		return Maintenance{}, false, err
	}
	return m, true, nil
}
