package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

const (
	// SessionTTL is the absolute lifetime of a session.
	SessionTTL = 30 * 24 * time.Hour
	// SessionIdle is how long a session may go unused before it expires.
	SessionIdle = 7 * 24 * time.Hour
	// sessionTouchEvery limits how often last_seen_at is written.
	sessionTouchEvery = 5 * time.Minute
	// MaxSessionsPerUser caps live sessions; the oldest are dropped.
	MaxSessionsPerUser = 20
)

// sessionDead is the condition for an expired or idle session. Arguments:
// now (ms), now minus SessionIdle (ms); see deadArgs.
const sessionDead = `(expires_at <= ? OR last_seen_at < ?)`

func deadArgs(now time.Time) []any {
	return []any{now.UnixMilli(), now.Add(-SessionIdle).UnixMilli()}
}

// Auth is an authenticated request's identity.
type Auth struct {
	User        User
	Workspace   Workspace
	WS          *WS
	SessionHash string
	// AuthAt is when the session last authenticated for real (sign-in or a
	// reauth); nil for sessions created before schema v3.
	AuthAt *time.Time
	// HasPassword is false for a social-only account.
	HasPassword bool
}

// CreateSession starts a session for userID and returns the raw token, which
// is never stored. Sessions beyond MaxSessionsPerUser are deleted, oldest
// first.
func (s *Store) CreateSession(ctx context.Context, userID int64, now time.Time) (string, error) {
	raw, hash := newSecret()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	ms := now.UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, last_seen_at, expires_at, auth_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hash, userID, ms, ms, now.Add(SessionTTL).UnixMilli(), ms); err != nil {
		return "", err
	}
	// Dead sessions never count toward the cap.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND `+sessionDead,
		append([]any{userID}, deadArgs(now)...)...); err != nil {
		return "", err
	}
	// Keep the new session plus the newest MaxSessionsPerUser-1 others.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND token_hash <> ? AND token_hash NOT IN (
			SELECT token_hash FROM sessions WHERE user_id = ? AND token_hash <> ?
			ORDER BY created_at DESC, token_hash LIMIT ?)`,
		userID, hash, userID, hash, MaxSessionsPerUser-1); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return raw, nil
}

// SessionAuth resolves a raw session token to its user and workspace. A
// missing, expired or idle session, or a disabled or unverified user, is
// ErrNotFound. last_seen_at is refreshed at most every sessionTouchEvery.
func (s *Store) SessionAuth(ctx context.Context, raw string, now time.Time) (Auth, error) {
	hash := hashSecret(raw)
	var a Auth
	var wsCreated, lastSeen int64
	var authAt sql.NullInt64
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT u.id, u.email, u.is_operator, u.verified_at, u.disabled_at, u.created_at, u.time_zone, u.detected_tz,
			w.id, w.name, w.slug, w.created_at, s.last_seen_at, s.auth_at, u.password_hash <> ''
		 FROM sessions s
		 JOIN users u ON u.id = s.user_id
		 JOIN memberships m ON m.user_id = u.id AND m.role = 'owner'
		 JOIN workspaces w ON w.id = m.workspace_id
		 WHERE s.token_hash = ? AND s.expires_at > ? AND s.last_seen_at >= ?
			AND u.disabled_at IS NULL AND u.verified_at IS NOT NULL
		 ORDER BY w.id LIMIT 1`,
		hash, now.UnixMilli(), now.Add(-SessionIdle).UnixMilli()),
		&a.Workspace.ID, &a.Workspace.Name, &a.Workspace.Slug, &wsCreated, &lastSeen, &authAt, &a.HasPassword)
	if errors.Is(err, sql.ErrNoRows) {
		return Auth{}, ErrNotFound
	}
	if err != nil {
		return Auth{}, err
	}
	a.Workspace.CreatedAt = time.UnixMilli(wsCreated).UTC()
	a.User, a.SessionHash, a.AuthAt = u, hash, msPtr(authAt)
	a.WS = s.Workspace(a.Workspace.ID)
	if now.Sub(time.UnixMilli(lastSeen)) >= sessionTouchEvery {
		// Best effort: the touch only extends the idle window, so a failed
		// or skipped touch makes the session expire sooner, the safe
		// direction. A valid session must not be rejected because of it.
		if err := s.touchSession(ctx, hash, u.ID, now); err != nil {
			slog.Warn("store: session touch failed", "err", err)
		}
	}
	return a, nil
}

// touchSession refreshes last_seen_at on the session and its user in one
// transaction, after the read in SessionAuth has finished (single
// connection).
func (s *Store) touchSession(ctx context.Context, hash string, userID int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ms := now.UnixMilli()
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`, ms, hash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET last_seen_at = ? WHERE id = ?`, ms, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkReauth records that the session just authenticated for real. sessionHash
// is Auth.SessionHash; an unknown session is ErrNotFound.
func (s *Store) MarkReauth(ctx context.Context, sessionHash string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET auth_at = ? WHERE token_hash = ?`, now.UnixMilli(), sessionHash)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeleteSession revokes one session. A missing session is not an error.
func (s *Store) DeleteSession(ctx context.Context, raw string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashSecret(raw))
	return err
}

// DeleteUserSessions revokes every session of a user.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

// PruneSessions deletes expired and idle sessions and returns how many.
func (s *Store) PruneSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE `+sessionDead, deadArgs(now)...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
