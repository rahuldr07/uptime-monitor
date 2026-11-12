package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	// ErrIdentityTaken is returned when the (provider, subject) is linked to
	// another user, or the user already has an identity for the provider.
	ErrIdentityTaken = errors.New("identity is already linked")
	// ErrLastSignInMethod is returned when unlinking would leave an account
	// with no password and no other identity.
	ErrLastSignInMethod = errors.New("cannot remove the last sign-in method")
	// ErrPasswordSet is returned by SetFirstPassword when the account
	// already has a password.
	ErrPasswordSet = errors.New("account already has a password")
)

// NewIdentity is an identity to link while creating an account.
type NewIdentity struct{ Provider, Subject, Email string }

// Identity is a linked social sign-in. Email is the provider's verified
// address at link time, for display only.
type Identity struct {
	Provider, Subject, Email string
	UserID                   int64
	CreatedAt                time.Time
	LastUsedAt               *time.Time
}

func isIdentityTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: identities.")
}

func insertIdentity(ctx context.Context, q querier, userID int64, provider, subject, email string, now time.Time) error {
	_, err := q.ExecContext(ctx,
		`INSERT INTO identities (provider, subject, user_id, email, created_at) VALUES (?, ?, ?, ?, ?)`,
		provider, subject, userID, email, now.UnixMilli())
	if isIdentityTaken(err) {
		return ErrIdentityTaken
	}
	return err
}

// IdentityUser returns the user an identity is linked to, or ErrNotFound.
// Lookups use (provider, subject), never the email.
func (s *Store) IdentityUser(ctx context.Context, provider, subject string) (User, error) {
	u, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT u.id, u.email, u.is_operator, u.verified_at, u.disabled_at, u.created_at
		 FROM identities i JOIN users u ON u.id = i.user_id
		 WHERE i.provider = ? AND i.subject = ?`, provider, subject))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// LinkIdentity links a provider identity to a user. ErrIdentityTaken means
// the identity belongs to another user or the user already has one for the
// provider.
func (s *Store) LinkIdentity(ctx context.Context, userID int64, provider, subject, email string, now time.Time) error {
	return insertIdentity(ctx, s.db, userID, provider, subject, email, now)
}

// UnlinkIdentity removes a user's identity for a provider. It returns
// ErrNotFound if there is none and ErrLastSignInMethod if the user would be
// left with no password and no other identity; the check and the delete share
// one transaction.
func (s *Store) UnlinkIdentity(ctx context.Context, userID int64, provider string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hasPW bool
	if err := tx.QueryRowContext(ctx, `SELECT password_hash <> '' FROM users WHERE id = ?`, userID).Scan(&hasPW); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var mine, others int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(provider = ?), 0), COALESCE(SUM(provider <> ?), 0) FROM identities WHERE user_id = ?`,
		provider, provider, userID).Scan(&mine, &others); err != nil {
		return err
	}
	if mine == 0 {
		return ErrNotFound
	}
	if !hasPW && others == 0 {
		return ErrLastSignInMethod
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE user_id = ? AND provider = ?`, userID, provider); err != nil {
		return err
	}
	return tx.Commit()
}

// Identities lists a user's linked identities, oldest first.
func (s *Store) Identities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider, subject, email, user_id, created_at, last_used_at FROM identities
		 WHERE user_id = ? ORDER BY created_at, provider`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&i.Provider, &i.Subject, &i.Email, &i.UserID, &created, &used); err != nil {
			return nil, err
		}
		i.CreatedAt, i.LastUsedAt = time.UnixMilli(created).UTC(), msPtr(used)
		out = append(out, i)
	}
	return out, rows.Err()
}

// TouchIdentity records a sign-in through an identity. An unknown identity is
// not an error.
func (s *Store) TouchIdentity(ctx context.Context, provider, subject string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE identities SET last_used_at = ? WHERE provider = ? AND subject = ?`,
		now.UnixMilli(), provider, subject)
	return err
}

// HasPassword reports whether the user has a password (an empty hash means a
// social-only account). An unknown user is ErrNotFound.
func (s *Store) HasPassword(ctx context.Context, userID int64) (bool, error) {
	var has bool
	err := s.db.QueryRowContext(ctx, `SELECT password_hash <> '' FROM users WHERE id = ?`, userID).Scan(&has)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return has, err
}

// SetFirstPassword sets a password only when the account has none, with the
// same session and token revocation as SetPassword. Otherwise it returns
// ErrPasswordSet and changes nothing; an unknown user is ErrNotFound.
func (s *Store) SetFirstPassword(ctx context.Context, userID int64, phc string, keepSessionHash string) error {
	if phc == "" {
		return errors.New("store: empty password hash")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var cur string
	if err := tx.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&cur); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if cur != "" {
		return ErrPasswordSet
	}
	if err := setPassword(ctx, tx, userID, phc, keepSessionHash); err != nil {
		return err
	}
	return tx.Commit()
}
