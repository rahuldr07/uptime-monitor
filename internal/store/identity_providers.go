package store

import (
	"context"
	"strings"
)

// IdentityProviders returns the providers each of the given users has
// connected, sorted by provider name, in one query. Users with none are
// absent from the map.
func (s *Store) IdentityProviders(ctx context.Context, userIDs []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, provider FROM identities WHERE user_id IN (?`+strings.Repeat(",?", len(userIDs)-1)+`) ORDER BY user_id, provider`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}
