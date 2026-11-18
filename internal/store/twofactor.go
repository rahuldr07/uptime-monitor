package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const (
	// PendingTOTPTTL is how long an unconfirmed 2FA secret stays valid.
	PendingTOTPTTL = 15 * time.Minute
	// PendingLoginTTL is how long a sign-in waits for its code.
	PendingLoginTTL = 5 * time.Minute
	// PendingLoginTries is how many code attempts one pending sign-in allows.
	PendingLoginTries = 5
)

// BeginTOTP stores secret as the user's pending (unconfirmed) 2FA secret,
// replacing any earlier one. It is ErrNotFound when 2FA is already on.
func (s *Store) BeginTOTP(ctx context.Context, userID int64, secret string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_pending = ?, totp_pending_at = ? WHERE id = ? AND totp_enabled_at IS NULL`,
		secret, now.UnixMilli(), userID)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// PendingTOTP returns the pending secret, or ErrNotFound if there is none or
// it is older than PendingTOTPTTL.
func (s *Store) PendingTOTP(ctx context.Context, userID int64, now time.Time) (string, error) {
	var secret string
	err := s.db.QueryRowContext(ctx,
		`SELECT totp_pending FROM users WHERE id = ? AND totp_pending <> '' AND totp_pending_at > ?`,
		userID, now.Add(-PendingTOTPTTL).UnixMilli()).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return secret, err
}

// insertRecoveryHashes replaces the user's recovery codes with hashes.
func insertRecoveryHashes(ctx context.Context, tx *sql.Tx, userID int64, hashes []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, h := range hashes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (user_id, code_hash) VALUES (?, ?)`, userID, h); err != nil {
			return err
		}
	}
	return nil
}

// deleteOtherSessions revokes the user's sessions except keepHash ("" keeps none).
func deleteOtherSessions(ctx context.Context, tx *sql.Tx, userID int64, keepHash string) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash <> ?`, userID, keepHash)
	return err
}

// EnableTOTP turns 2FA on in one transaction: the pending secret (still valid
// at now) becomes the secret, totp_last_step is raised to step, the recovery
// codes are replaced by codeHashes, the kept session gets mfa_at, every other
// session and all of the user's pending sign-ins are deleted. It is
// ErrNotFound if 2FA is already on or there is no valid pending secret.
func (s *Store) EnableTOTP(ctx context.Context, userID int64, step int64, codeHashes []string, keepSessionHash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET totp_secret = totp_pending, totp_pending = '', totp_pending_at = NULL,
			totp_enabled_at = ?, totp_last_step = MAX(totp_last_step, ?)
		 WHERE id = ? AND totp_enabled_at IS NULL AND totp_pending <> '' AND totp_pending_at > ?`,
		now.UnixMilli(), step, userID, now.Add(-PendingTOTPTTL).UnixMilli())
	if err != nil {
		return err
	}
	if err := requireRow(res); err != nil {
		return err
	}
	if err := insertRecoveryHashes(ctx, tx, userID, codeHashes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET mfa_at = ? WHERE token_hash = ? AND user_id = ?`,
		now.UnixMilli(), keepSessionHash, userID); err != nil {
		return err
	}
	if err := deleteOtherSessions(ctx, tx, userID, keepSessionHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_logins WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// DisableTOTP turns 2FA off (or resets it): secret, pending secret and
// recovery codes are cleared, other sessions (all when keepSessionHash is "")
// and pending sign-ins are deleted. totp_last_step is kept, so an old code
// can never be replayed after re-enabling.
func (s *Store) DisableTOTP(ctx context.Context, userID int64, keepSessionHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE users SET totp_secret = '', totp_pending = '', totp_pending_at = NULL, totp_enabled_at = NULL WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	if err := requireRow(res); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if err := deleteOtherSessions(ctx, tx, userID, keepSessionHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_logins WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// TOTPSecret returns the user's active secret and whether 2FA is on.
func (s *Store) TOTPSecret(ctx context.Context, userID int64) (secret string, enabled bool, err error) {
	var at sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT totp_secret, totp_enabled_at FROM users WHERE id = ?`, userID).Scan(&secret, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, err
	}
	return secret, at.Valid, nil
}

// AcceptTOTPStep records step as used with one conditional UPDATE and reports
// whether this caller won: of any number of concurrent calls with the same
// step exactly one is true, and a step at or below the last one never is.
func (s *Store) AcceptTOTPStep(ctx context.Context, userID int64, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_enabled_at IS NOT NULL AND totp_last_step < ?`,
		step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// UseRecoveryCode marks a recovery code (by hash) used with one conditional
// UPDATE; of concurrent callers exactly one gets true.
func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, hash string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE recovery_codes SET used_at = ? WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		now.UnixMilli(), userID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecoveryCodesLeft counts the user's unused recovery codes.
func (s *Store) RecoveryCodesLeft(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&n)
	return n, err
}

// ReplaceRecoveryCodes swaps all of a 2FA user's recovery codes for hashes and
// signs out every session but keepSessionHash. It is ErrNotFound when 2FA is off.
func (s *Store) ReplaceRecoveryCodes(ctx context.Context, userID int64, hashes []string, keepSessionHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var on int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ? AND totp_enabled_at IS NOT NULL`, userID).Scan(&on); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := insertRecoveryHashes(ctx, tx, userID, hashes); err != nil {
		return err
	}
	if err := deleteOtherSessions(ctx, tx, userID, keepSessionHash); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingLogin is a password or social sign-in that is waiting for its
// two-factor code. No session exists yet.
type PendingLogin struct {
	UserID int64
	Method string // password | google | github
	Next   string
	// TOTPEnabledAt is the user's 2FA enable time when the row was made.
	TOTPEnabledAt time.Time
	CreatedAt     time.Time
	ExpiresAt     time.Time
	Tries         int
}

// pendingLive is the condition for a pending sign-in that can still be
// completed: unused, unexpired, and its user still has the same 2FA turned
// on, so turning 2FA off or resetting it invalidates the row. Arguments:
// token hash, now.
const pendingLive = `token_hash = ? AND used_at IS NULL AND expires_at > ?
	AND totp_enabled_at = (SELECT totp_enabled_at FROM users WHERE id = pending_logins.user_id)`

const pendingCols = `user_id, method, next, totp_enabled_at, created_at, expires_at, tries`

func scanPending(sc scanner) (PendingLogin, error) {
	var p PendingLogin
	var enabled, created, expires int64
	if err := sc.Scan(&p.UserID, &p.Method, &p.Next, &enabled, &created, &expires, &p.Tries); err != nil {
		return PendingLogin{}, err
	}
	p.TOTPEnabledAt = time.UnixMilli(enabled).UTC()
	p.CreatedAt = time.UnixMilli(created).UTC()
	p.ExpiresAt = time.UnixMilli(expires).UTC()
	return p, nil
}

// CreatePendingLogin records a sign-in awaiting a code and returns the raw
// token, which is never stored. totpEnabledAt is the user's TOTPEnabledAt.
func (s *Store) CreatePendingLogin(ctx context.Context, userID int64, method, next string, totpEnabledAt, now time.Time) (string, error) {
	raw, hash := newSecret()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_logins (token_hash, user_id, method, next, totp_enabled_at, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hash, userID, method, next, totpEnabledAt.UnixMilli(), now.UnixMilli(), now.Add(PendingLoginTTL).UnixMilli()); err != nil {
		return "", err
	}
	return raw, nil
}

// PendingLogin looks a pending sign-in up without using a try. ErrNotFound if
// it is unknown, used, expired, or its user's 2FA changed.
func (s *Store) PendingLogin(ctx context.Context, raw string, now time.Time) (PendingLogin, error) {
	p, err := scanPending(s.db.QueryRowContext(ctx,
		`SELECT `+pendingCols+` FROM pending_logins WHERE `+pendingLive, hashSecret(raw), now.UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return PendingLogin{}, ErrNotFound
	}
	return p, err
}

// TryPendingLogin uses one of the row's tries with a single conditional
// UPDATE, so concurrent submissions cannot exceed PendingLoginTries. It is
// ErrNotFound for the same reasons as PendingLogin, or when no tries are left.
func (s *Store) TryPendingLogin(ctx context.Context, raw string, now time.Time) (PendingLogin, error) {
	hash := hashSecret(raw)
	res, err := s.db.ExecContext(ctx,
		`UPDATE pending_logins SET tries = tries + 1 WHERE `+pendingLive+` AND tries < ?`,
		hash, now.UnixMilli(), PendingLoginTries)
	if err != nil {
		return PendingLogin{}, err
	}
	if err := requireRow(res); err != nil {
		return PendingLogin{}, err
	}
	return scanPending(s.db.QueryRowContext(ctx, `SELECT `+pendingCols+` FROM pending_logins WHERE token_hash = ?`, hash))
}

// FinishPendingLogin marks the row used, once: of concurrent callers exactly
// one succeeds, the rest get ErrNotFound.
func (s *Store) FinishPendingLogin(ctx context.Context, raw string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE pending_logins SET used_at = ? WHERE `+pendingLive,
		now.UnixMilli(), hashSecret(raw), now.UnixMilli())
	if err != nil {
		return err
	}
	return requireRow(res)
}

// DeletePendingLogin removes a pending sign-in. A missing row is not an error.
func (s *Store) DeletePendingLogin(ctx context.Context, raw string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM pending_logins WHERE token_hash = ?`, hashSecret(raw))
	return err
}

// PrunePendingLogins deletes expired and used pending sign-ins and returns how many.
func (s *Store) PrunePendingLogins(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pending_logins WHERE expires_at <= ? OR used_at IS NOT NULL`, now.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
