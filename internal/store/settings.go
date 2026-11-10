package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"uptimemon/internal/config"
)

// GetSetting returns a setting value, or ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	return s.Workspace(legacyWorkspaceID).GetSetting(ctx, key)
}

// GetSetting returns one of the workspace's settings, or ErrNotFound.
func (w *WS) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := w.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE workspace_id = ? AND key = ?`, w.id, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetSetting upserts a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.Workspace(legacyWorkspaceID).SetSetting(ctx, key, value)
}

// SetSetting upserts one of the workspace's settings.
func (w *WS) SetSetting(ctx context.Context, key, value string) error {
	_, err := w.db.ExecContext(ctx,
		`INSERT INTO settings (workspace_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(workspace_id, key) DO UPDATE SET value = excluded.value`, w.id, key, value)
	return err
}

// Notify returns the workspace's alert channels. With none stored it returns
// the zero value and no error.
func (w *WS) Notify(ctx context.Context) (config.Notify, error) {
	var n config.Notify
	raw, err := w.GetSetting(ctx, "notify")
	if errors.Is(err, ErrNotFound) {
		return n, nil
	}
	if err != nil {
		return n, err
	}
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		return config.Notify{}, err
	}
	return n, nil
}
