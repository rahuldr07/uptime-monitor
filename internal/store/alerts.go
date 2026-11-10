package store

import (
	"context"
	"time"
)

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
