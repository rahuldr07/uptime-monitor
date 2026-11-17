package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrOperator is returned when an operator would be disabled or deleted by
// the operator panel.
var ErrOperator = errors.New("store: operators cannot be disabled or deleted here")

// UserByID returns a user, or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// SetUserDisabled disables or re-enables a user in one transaction and
// returns the IDs of the workspaces it suspended or restored: those where the
// user is the only member, never OperatorWorkspaceID.
//
// Disabling an operator is ErrOperator. Disabling also deletes the user's
// sessions and unused verify and reset tokens. Enabling is allowed for
// operators, as the -enable-user recovery path. An unknown user is
// ErrNotFound.
func (s *Store) SetUserDisabled(ctx context.Context, userID int64, disabled bool, now time.Time) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var op bool
	err = tx.QueryRowContext(ctx, `SELECT is_operator <> 0 FROM users WHERE id = ?`, userID).Scan(&op)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if disabled && op {
		return nil, ErrOperator
	}
	// Disabling keeps any earlier timestamps; enabling clears them.
	if disabled {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = COALESCE(disabled_at, ?) WHERE id = ?`, now.UnixMilli(), userID); err != nil {
			return nil, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = NULL WHERE id = ?`, userID); err != nil {
		return nil, err
	}
	if disabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE user_id = ? AND kind IN ('reset', 'verify') AND used_at IS NULL`, userID); err != nil {
			return nil, err
		}
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT workspace_id FROM memberships m WHERE user_id = ? AND workspace_id <> ?
		 AND (SELECT COUNT(*) FROM memberships WHERE workspace_id = m.workspace_id) = 1
		 ORDER BY workspace_id`, userID, OperatorWorkspaceID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if disabled {
			_, err = tx.ExecContext(ctx, `UPDATE workspaces SET suspended_at = COALESCE(suspended_at, ?) WHERE id = ?`, now.UnixMilli(), id)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE workspaces SET suspended_at = NULL WHERE id = ?`, id)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// UsersPerPage is the page size of ListUsers.
const UsersPerPage = 50

// UserQuery filters ListUsers. Email matches anywhere in the address and Host
// anywhere in a monitored URL; both are literal, not patterns. Page is
// 0-based.
type UserQuery struct {
	Email, Host string
	Page        int
}

// UserRow is a user with the details the operator panel shows. The workspace
// is the first one the user owns, by ID; WorkspaceID is 0 if there is none.
// Monitors counts the targets of every workspace the user owns.
type UserRow struct {
	User
	LastSeenAt    *time.Time
	WorkspaceID   int64
	WorkspaceName string
	WorkspaceSlug string
	Monitors      int
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likeContains builds a LIKE pattern matching s anywhere, for ESCAPE '\'.
func likeContains(s string) string { return "%" + likeEscaper.Replace(s) + "%" }

const ownedWorkspaces = `(SELECT workspace_id FROM memberships WHERE user_id = u.id AND role = 'owner')`

// ListUsers returns one page of users, newest first, and the total number
// matching the filter.
func (s *Store) ListUsers(ctx context.Context, q UserQuery) ([]UserRow, int, error) {
	where, args := " WHERE 1=1", []any{}
	if e := strings.ToLower(strings.TrimSpace(q.Email)); e != "" {
		where += ` AND u.email LIKE ? ESCAPE '\'`
		args = append(args, likeContains(e))
	}
	if h := strings.TrimSpace(q.Host); h != "" {
		where += ` AND EXISTS (SELECT 1 FROM targets t WHERE t.workspace_id IN ` + ownedWorkspaces + ` AND t.url LIKE ? ESCAPE '\')`
		args = append(args, likeContains(h))
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users u`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	page := max(q.Page, 0)
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.email, u.is_operator, u.verified_at, u.disabled_at, u.created_at, u.time_zone, u.detected_tz,
			u.last_seen_at, COALESCE(w.id, 0), COALESCE(w.name, ''), COALESCE(w.slug, ''),
			(SELECT COUNT(*) FROM targets t WHERE t.workspace_id IN `+ownedWorkspaces+`)
		 FROM users u
		 LEFT JOIN workspaces w ON w.id = (SELECT MIN(workspace_id) FROM memberships WHERE user_id = u.id AND role = 'owner')`+
			where+` ORDER BY u.created_at DESC, u.id DESC LIMIT ? OFFSET ?`,
		append(args, UsersPerPage, page*UsersPerPage)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []UserRow
	for rows.Next() {
		var r UserRow
		var seen sql.NullInt64
		// scanUser scans the user columns, then the extras.
		r.User, err = scanUser(rows, &seen, &r.WorkspaceID, &r.WorkspaceName, &r.WorkspaceSlug, &r.Monitors)
		if err != nil {
			return nil, 0, err
		}
		r.LastSeenAt = msPtr(seen)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// Invite is a live, unused invite as the operator panel lists it.
type Invite struct {
	ID                   string // stored token_hash; cannot be used to sign up
	Email                string // "" when unbound
	CreatedAt, ExpiresAt time.Time
}

// ListInvites returns the unused, unexpired invites, newest first.
func (s *Store) ListInvites(ctx context.Context, now time.Time) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT token_hash, email, created_at, expires_at FROM tokens
		 WHERE kind = ? AND used_at IS NULL AND expires_at > ?
		 ORDER BY created_at DESC, rowid DESC`, TokenInvite, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var in Invite
		var email sql.NullString
		var created, expires int64
		if err := rows.Scan(&in.ID, &email, &created, &expires); err != nil {
			return nil, err
		}
		in.Email = email.String
		in.CreatedAt = time.UnixMilli(created).UTC()
		in.ExpiresAt = time.UnixMilli(expires).UTC()
		out = append(out, in)
	}
	return out, rows.Err()
}

// RevokeInvite deletes an unused invite by its ListInvites ID. It is
// ErrNotFound for anything else, including tokens of other kinds.
func (s *Store) RevokeInvite(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE token_hash = ? AND kind = ? AND used_at IS NULL`, id, TokenInvite)
	if err != nil {
		return err
	}
	return requireRow(res)
}
