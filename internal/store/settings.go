package store

import (
	"context"
	"database/sql"
	"errors"
)

// GetSetting returns a setting value, or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE workspace_id = ? AND key = ?`, legacyWorkspaceID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (workspace_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(workspace_id, key) DO UPDATE SET value = excluded.value`, legacyWorkspaceID, key, value)
	return err
}
