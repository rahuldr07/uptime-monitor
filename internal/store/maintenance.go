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
