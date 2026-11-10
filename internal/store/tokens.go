package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// newSecret returns a fresh random secret and the hash that is stored in its
// place. It is the only place secrets are generated.
func newSecret() (raw, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashSecret(raw)
}

// randomBase32 returns n random lowercase base32 characters (a-z, 2-7).
func randomBase32(n int) string {
	b := make([]byte, (n*5+7)/8)
	if _, err := rand.Read(b); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:n]
}

// hashSecret returns the hex SHA-256 of a raw secret. It is the only place
// secrets are hashed.
func hashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TokenKind is the purpose of a single-use token.
type TokenKind string

const (
	TokenSetup TokenKind = "setup"
	TokenReset TokenKind = "reset"
	// TokenInvite admits one new sign-up. It has no user and may be bound to
	// an email.
	TokenInvite TokenKind = "invite"
)

// Token is a validated single-use token.
type Token struct {
	Kind                 TokenKind
	UserID               int64
	Email                string // invite only; empty when not bound to an address
	CreatedAt, ExpiresAt time.Time
}

// tokenLive is the condition for an unused, unexpired token of a kind whose
// holder, for reset tokens, is not disabled. Arguments: kind, now.
const tokenLive = `kind = ? AND used_at IS NULL AND expires_at > ?
	AND (kind <> 'reset' OR user_id IN (SELECT id FROM users WHERE disabled_at IS NULL))`

// CreateToken issues a token of kind for userID (0 for setup) and returns the
// raw value, which is never stored. Earlier unused tokens of the same kind
// for that user (every unused setup token) are deleted.
func (s *Store) CreateToken(ctx context.Context, kind TokenKind, userID int64, ttl time.Duration, now time.Time) (string, error) {
	switch kind {
	case TokenSetup:
		if userID != 0 {
			return "", errors.New("store: setup token takes no user")
		}
	case TokenReset:
		if userID <= 0 {
			return "", errors.New("store: reset token needs a user")
		}
	default:
		return "", fmt.Errorf("store: unsupported token kind %q", kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if kind == TokenSetup {
		_, err = tx.ExecContext(ctx, `DELETE FROM tokens WHERE kind = ? AND used_at IS NULL`, kind)
	} else {
		_, err = tx.ExecContext(ctx, `DELETE FROM tokens WHERE kind = ? AND user_id = ? AND used_at IS NULL`, kind, userID)
	}
	if err != nil {
		return "", err
	}
	raw, hash := newSecret()
	var uid any
	if userID != 0 {
		uid = userID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tokens (token_hash, kind, user_id, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hash, kind, uid, now.UnixMilli(), now.Add(ttl).UnixMilli()); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return raw, nil
}

// CreateInvite issues an invite token and returns the raw value. email, when
// not empty, is normalised and binds the invite to that address. Existing
// invites are left alone.
func (s *Store) CreateInvite(ctx context.Context, email string, ttl time.Duration, now time.Time) (string, error) {
	var bound any
	if email != "" {
		e, err := NormalizeEmail(email)
		if err != nil {
			return "", err
		}
		bound = e
	}
	raw, hash := newSecret()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (token_hash, kind, user_id, email, created_at, expires_at) VALUES (?, ?, NULL, ?, ?, ?)`,
		hash, TokenInvite, bound, now.UnixMilli(), now.Add(ttl).UnixMilli()); err != nil {
		return "", err
	}
	return raw, nil
}

func scanToken(sc scanner) (Token, error) {
	var t Token
	var uid sql.NullInt64
	var email sql.NullString
	var created, expires int64
	if err := sc.Scan(&t.Kind, &uid, &email, &created, &expires); err != nil {
		return Token{}, err
	}
	t.UserID = uid.Int64
	t.Email = email.String
	t.CreatedAt = time.UnixMilli(created).UTC()
	t.ExpiresAt = time.UnixMilli(expires).UTC()
	return t, nil
}

// PeekToken checks a token without consuming it. It returns ErrNotFound when
// the token is unknown, of another kind, used, expired, or a reset token of a
// disabled user.
func (s *Store) PeekToken(ctx context.Context, kind TokenKind, raw string, now time.Time) (Token, error) {
	return peekToken(ctx, s.db, kind, raw, now)
}

func peekToken(ctx context.Context, q querier, kind TokenKind, raw string, now time.Time) (Token, error) {
	t, err := scanToken(q.QueryRowContext(ctx,
		`SELECT kind, user_id, email, created_at, expires_at FROM tokens WHERE token_hash = ? AND `+tokenLive,
		hashSecret(raw), kind, now.UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, ErrNotFound
	}
	return t, err
}

// consumeToken marks a token used with one conditional UPDATE, so exactly one
// of any number of concurrent callers succeeds; the others get ErrNotFound.
func consumeToken(ctx context.Context, q querier, kind TokenKind, raw string, now time.Time) (Token, error) {
	hash := hashSecret(raw)
	res, err := q.ExecContext(ctx, `UPDATE tokens SET used_at = ? WHERE token_hash = ? AND `+tokenLive,
		now.UnixMilli(), hash, kind, now.UnixMilli())
	if err != nil {
		return Token{}, err
	}
	if err := requireRow(res); err != nil {
		return Token{}, err
	}
	return scanToken(q.QueryRowContext(ctx,
		`SELECT kind, user_id, email, created_at, expires_at FROM tokens WHERE token_hash = ?`, hash))
}

// PruneTokens deletes tokens that are used or expired and older than 7 days.
func (s *Store) PruneTokens(ctx context.Context, now time.Time) (int64, error) {
	cutoff := now.Add(-7 * 24 * time.Hour).UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE created_at < ? AND (used_at IS NOT NULL OR expires_at < ?)`, cutoff, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
