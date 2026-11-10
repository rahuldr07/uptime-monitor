package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrLastOperator is returned when deleting the only operator.
var ErrLastOperator = errors.New("the last operator cannot be deleted")

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
	var op bool
	err = tx.QueryRowContext(ctx, `SELECT is_operator <> 0 FROM users WHERE id = ?`, userID).Scan(&op)
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}
