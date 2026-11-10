package store

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/mail"
	"strings"
	"time"
	"unicode"
)

var (
	// ErrSetupDone is returned by CompleteSetup once a user exists.
	ErrSetupDone = errors.New("setup is already complete")
	// ErrEmailTaken is returned when an email is already registered.
	ErrEmailTaken = errors.New("email is already registered")
	// ErrInvalidEmail is returned by NormalizeEmail.
	ErrInvalidEmail = errors.New("Enter a valid email address.")
)

// User is an account holder.
type User struct {
	ID                     int64
	Email                  string
	IsOperator             bool
	VerifiedAt, DisabledAt *time.Time
	CreatedAt              time.Time
}

// NormalizeEmail validates an address and returns it trimmed and lowercased.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 254 {
		return "", ErrInvalidEmail
	}
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return "", ErrInvalidEmail
		}
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return "", ErrInvalidEmail
	}
	at := strings.LastIndexByte(s, '@')
	domain := s[at+1:]
	if strings.Contains(s, "@[") || !strings.Contains(domain, ".") || net.ParseIP(domain) != nil {
		return "", ErrInvalidEmail
	}
	return s, nil
}

// isEmailTaken reports whether err is the users.email UNIQUE violation. It is
// the only place that inspects driver error text.
func isEmailTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: users.email")
}

const userCols = `id, email, is_operator, verified_at, disabled_at, created_at`

func msPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}

func scanUser(sc scanner, extra ...any) (User, error) {
	var u User
	var op int
	var verified, disabled sql.NullInt64
	var created int64
	dest := append([]any{&u.ID, &u.Email, &op, &verified, &disabled, &created}, extra...)
	if err := sc.Scan(dest...); err != nil {
		return User{}, err
	}
	u.IsOperator = op != 0
	u.VerifiedAt, u.DisabledAt = msPtr(verified), msPtr(disabled)
	u.CreatedAt = time.UnixMilli(created).UTC()
	return u, nil
}

// CountUsers returns the number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// UserByEmail returns the user and their password hash. The email is
// normalized first; an invalid address is ErrNotFound.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, string, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, "", ErrNotFound
	}
	var phc string
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userCols+`, password_hash FROM users WHERE email = ?`, email), &phc)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	return u, phc, err
}

// UserByID returns a user or ErrNotFound.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// IsUpgradedInstall reports whether this is a pre-accounts database: no users
// yet, but workspace 1 already holds targets, settings or maintenance.
func (s *Store) IsUpgradedInstall(ctx context.Context) (bool, error) {
	return isUpgradedInstall(ctx, s.db)
}

func isUpgradedInstall(ctx context.Context, q querier) (bool, error) {
	var has bool
	err := q.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users) = 0 AND (
			EXISTS (SELECT 1 FROM targets WHERE workspace_id = 1) OR
			EXISTS (SELECT 1 FROM settings WHERE workspace_id = 1) OR
			EXISTS (SELECT 1 FROM maintenance WHERE workspace_id = 1))`).Scan(&has)
	return has, err
}

// SetupInput describes the first-run setup. An empty SetupToken means the
// password-unlock path, where the caller has already checked the password.
type SetupInput struct {
	Email, PasswordHash, WorkspaceName, SetupToken string
	// PasswordUnlocked authorizes the password-unlock path; the caller sets
	// it only after verifying admin_password.
	PasswordUnlocked bool
	Now              time.Time
}

func insertUser(ctx context.Context, q querier, email, phc string, operator, verified bool, now time.Time) (User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	now = time.UnixMilli(now.UnixMilli()).UTC()
	var ver any
	if verified {
		ver = now.UnixMilli()
	}
	res, err := q.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, is_operator, verified_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		email, phc, boolToInt(operator), ver, now.UnixMilli())
	if isEmailTaken(err) {
		return User{}, ErrEmailTaken
	}
	if err != nil {
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	u := User{ID: id, Email: email, IsOperator: operator, CreatedAt: now}
	if verified {
		t := now
		u.VerifiedAt = &t
	}
	return u, nil
}

func insertOwner(ctx context.Context, q querier, userID, workspaceID int64, now time.Time) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO memberships (user_id, workspace_id, role, created_at) VALUES (?, ?, 'owner', ?)`,
		userID, workspaceID, now.UnixMilli())
	return err
}

// CompleteSetup creates the first user, an operator, and adopts workspace 1.
//
// Authorization is explicit: either SetupToken is set (the token is consumed
// inside the transaction) or PasswordUnlocked is true, in which case the
// caller must already have verified admin_password and the upgraded-install
// condition is re-checked inside the transaction. With neither, it returns
// ErrNotFound and changes nothing.
//
// The no-user check runs inside the transaction, so of any concurrent calls
// exactly one succeeds and the rest get ErrSetupDone.
func (s *Store) CompleteSetup(ctx context.Context, in SetupInput) (User, error) {
	if in.SetupToken == "" && !in.PasswordUnlocked {
		return User{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return User{}, err
	}
	if n > 0 {
		return User{}, ErrSetupDone
	}
	if in.PasswordUnlocked {
		up, err := isUpgradedInstall(ctx, tx)
		if err != nil {
			return User{}, err
		}
		if !up {
			return User{}, ErrNotFound
		}
	}
	if in.SetupToken != "" {
		if _, err := consumeToken(ctx, tx, TokenSetup, in.SetupToken, in.Now); err != nil {
			return User{}, err
		}
	}
	u, err := insertUser(ctx, tx, in.Email, in.PasswordHash, true, true, in.Now)
	if err != nil {
		return User{}, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE workspaces SET name = ? WHERE id = 1`, in.WorkspaceName)
	if err != nil {
		return User{}, err
	}
	if err := requireRow(res); err != nil {
		return User{}, err
	}
	if err := insertOwner(ctx, tx, u.ID, 1, in.Now); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return u, nil
}

// AccountInput describes a new user with a workspace of their own.
type AccountInput struct {
	Email, PasswordHash, WorkspaceName, Slug string
	Operator                                 bool
	Now                                      time.Time
}

// CreateAccount creates a user, a new workspace and the owner membership in
// one transaction.
func (s *Store) CreateAccount(ctx context.Context, in AccountInput) (User, *WS, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, nil, err
	}
	defer tx.Rollback()
	u, err := insertUser(ctx, tx, in.Email, in.PasswordHash, in.Operator, false, in.Now)
	if err != nil {
		return User{}, nil, err
	}
	ws, err := createWorkspace(ctx, tx, in.WorkspaceName, in.Slug, in.Now)
	if err != nil {
		return User{}, nil, err
	}
	if err := insertOwner(ctx, tx, u.ID, ws.ID, in.Now); err != nil {
		return User{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, nil, err
	}
	return u, s.Workspace(ws.ID), nil
}

// SetPassword replaces a user's password hash, deletes all their sessions
// except keepSessionHash (empty deletes all), and deletes their unused reset
// tokens.
func (s *Store) SetPassword(ctx context.Context, userID int64, phc string, keepSessionHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, phc, userID)
	if err != nil {
		return err
	}
	if err := requireRow(res); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepSessionHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE user_id = ? AND kind = 'reset' AND used_at IS NULL`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkVerified sets verified_at if it is not already set.
func (s *Store) MarkVerified(ctx context.Context, userID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET verified_at = ? WHERE id = ? AND verified_at IS NULL`, now.UnixMilli(), userID)
	return err
}

// UserWorkspace returns the single workspace the user owns.
func (s *Store) UserWorkspace(ctx context.Context, userID int64) (*WS, Workspace, error) {
	ws, err := scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT w.id, w.name, w.slug, w.created_at FROM workspaces w
		 JOIN memberships m ON m.workspace_id = w.id
		 WHERE m.user_id = ? AND m.role = 'owner' ORDER BY w.id LIMIT 1`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Workspace{}, ErrNotFound
	}
	if err != nil {
		return nil, Workspace{}, err
	}
	return s.Workspace(ws.ID), ws, nil
}
