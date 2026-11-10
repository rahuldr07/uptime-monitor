package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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

// GetIncident returns one of the workspace's incidents by id.
func (w *WS) GetIncident(ctx context.Context, id int64) (Incident, error) {
	in, err := scanIncident(w.db.QueryRowContext(ctx, incidentQuery+` WHERE i.id = ? AND i.`+inWS, id, w.id))
	if errors.Is(err, sql.ErrNoRows) {
		return Incident{}, ErrNotFound
	}
	return in, err
}

// AcknowledgeIncident marks one of the workspace's incidents as seen.
func (w *WS) AcknowledgeIncident(ctx context.Context, id int64, at time.Time) error {
	res, err := w.db.ExecContext(ctx,
		`UPDATE incidents SET acked_at = ? WHERE id = ? AND acked_at IS NULL AND `+inWS, at.UnixMilli(), id, w.id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// SetIncidentNote stores a note on one of the workspace's incidents.
func (w *WS) SetIncidentNote(ctx context.Context, id int64, note string) error {
	res, err := w.db.ExecContext(ctx, `UPDATE incidents SET note = ? WHERE id = ? AND `+inWS, note, id, w.id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// CountUnacknowledged counts the workspace's ongoing unacknowledged incidents.
func (w *WS) CountUnacknowledged(ctx context.Context) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM incidents WHERE ended_at IS NULL AND acked_at IS NULL AND `+inWS, w.id).Scan(&n)
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

// ListIncidents returns the workspace's incidents newest first.
func (w *WS) ListIncidents(ctx context.Context, f IncidentFilter) ([]Incident, error) {
	q := incidentQuery + ` WHERE i.` + inWS
	args := []any{w.id}
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
	rows, err := w.db.QueryContext(ctx, q, args...)
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

// CountIncidents counts the workspace's incidents started since the given time.
func (w *WS) CountIncidents(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM incidents WHERE started_at >= ? AND `+inWS, since.UnixMilli(), w.id).Scan(&n)
	return n, err
}
