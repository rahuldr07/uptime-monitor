package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrLastOperator is returned when deleting the only operator.
var ErrLastOperator = errors.New("the last operator cannot be deleted")

// IsLastOperator reports whether userID is the only active operator, i.e.
// whether DeleteAccount would refuse. It is a read-only precheck; DeleteAccount
// repeats the check in its transaction and stays the authority.
func (s *Store) IsLastOperator(ctx context.Context, userID int64) (bool, error) {
	var op bool
	err := s.db.QueryRowContext(ctx, `SELECT is_operator <> 0 FROM users WHERE id = ?`, userID).Scan(&op)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil || !op {
		return false, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE is_operator <> 0 AND disabled_at IS NULL AND id <> ?`, userID).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// DeleteAccount deletes a user in one transaction. Workspaces where the user
// is the only member are deleted with all their data (never the operator
// workspace, D-123); the IDs are returned.
// The caller deletes the user's sessions first. It returns ErrLastOperator
// for the only active operator and ErrNotFound for an unknown user.
func (s *Store) DeleteAccount(ctx context.Context, userID int64) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids, err := deleteAccount(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// PruneUnverified deletes non-operator users who never verified their address
// and were created before cutoff, with the workspaces where they are the only
// member, one transaction per user. The condition is re-checked inside each
// transaction, so an account verified meanwhile survives. It returns the
// deleted workspace IDs.
func (s *Store) PruneUnverified(ctx context.Context, cutoff time.Time) ([]int64, error) {
	const unverified = `is_operator = 0 AND verified_at IS NULL AND created_at < ?`
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM users WHERE `+unverified, cutoff.UnixMilli())
	if err != nil {
		return nil, err
	}
	var users []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		users = append(users, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var deleted []int64
	for _, uid := range users {
		ids, err := s.pruneUser(ctx, uid, unverified, cutoff)
		if err != nil {
			return deleted, err
		}
		deleted = append(deleted, ids...)
	}
	return deleted, nil
}

func (s *Store) pruneUser(ctx context.Context, uid int64, unverified string, cutoff time.Time) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var still int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ? AND `+unverified, uid, cutoff.UnixMilli()).Scan(&still); err != nil {
		return nil, err
	}
	if still == 0 {
		return nil, nil
	}
	ids, err := deleteAccount(ctx, tx, uid)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// deleteAccount is the deletion shared by DeleteAccount and PruneUnverified.
// It runs inside the caller's transaction.
func deleteAccount(ctx context.Context, tx querier, userID int64) ([]int64, error) {
	var op bool
	err := tx.QueryRowContext(ctx, `SELECT is_operator <> 0 FROM users WHERE id = ?`, userID).Scan(&op)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if op {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM users WHERE is_operator <> 0 AND disabled_at IS NULL AND id <> ?`, userID).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, ErrLastOperator
		}
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT workspace_id FROM memberships m WHERE user_id = ?
		 AND workspace_id <> ?
		 AND (SELECT COUNT(*) FROM memberships WHERE workspace_id = m.workspace_id) = 1`, userID, OperatorWorkspaceID)
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
		for _, tbl := range []string{"alerts", "incidents", "checks", "daily"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE `+inWS, id); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ?`, id); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID); err != nil {
		return nil, err
	}
	return ids, nil
}
