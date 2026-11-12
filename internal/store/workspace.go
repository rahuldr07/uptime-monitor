package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

// OperatorWorkspaceID is the workspace that always exists (D-123): it holds
// the data of a pre-accounts install and is the operator's own workspace.
// Public instances treat it specially: its targets are exempt from the
// workspace limits and from the host-rate check (D-124), and it backs the
// /status redirect (D-125).
const OperatorWorkspaceID int64 = 1

var (
	// ErrInvalidSlug is returned for a slug that breaks the slug rules.
	ErrInvalidSlug = errors.New("The address must be 3-40 characters: lowercase letters, digits and hyphens, not starting or ending with a hyphen.")
	// ErrSlugTaken is returned when another workspace already has the slug.
	ErrSlugTaken = errors.New("That address is already taken.")
	// ErrSlugUnavailable is returned when DeriveSlug finds no free slug.
	ErrSlugUnavailable = errors.New("store: no free slug")
)

// ValidSlug reports whether s is 3-40 characters of [a-z0-9-] with no leading
// or trailing hyphen.
func ValidSlug(s string) bool {
	if len(s) < 3 || len(s) > 40 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// isSlugTaken reports whether err is the workspaces.slug UNIQUE violation.
func isSlugTaken(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: workspaces.slug")
}

// DeriveSlug builds an unused slug from a workspace name: lowercase, runs of
// other characters become "-", ends trimmed, cut to 40; a result under 3
// characters becomes "ws-" plus 6 random base32 characters; on a collision
// "-2", "-3" and so on are appended, shortening the base to fit.
func DeriveSlug(ctx context.Context, q querier, name string) (string, error) {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		} else {
			dash = true
		}
	}
	base := strings.TrimRight(truncate(b.String(), 40), "-")
	if len(base) < 3 {
		base = "ws-" + randomBase32(6)
	}
	free := func(cand string) (bool, error) {
		var taken bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM workspaces WHERE slug = ?)`, cand).Scan(&taken)
		return !taken, err
	}
	for n := 1; n <= 20; n++ {
		cand := base
		if n > 1 {
			suffix := "-" + strconv.Itoa(n)
			cand = strings.TrimRight(truncate(base, 40-len(suffix)), "-") + suffix
		}
		if ok, err := free(cand); err != nil || ok {
			return cand, err
		}
	}
	for range 5 {
		suffix := "-" + randomBase32(4)
		cand := strings.TrimRight(truncate(base, 40-len(suffix)), "-") + suffix
		if ok, err := free(cand); err != nil || ok {
			return cand, err
		}
	}
	return "", ErrSlugUnavailable
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// SetSlug changes the workspace's public address.
func (w *WS) SetSlug(ctx context.Context, slug string) error {
	if !ValidSlug(slug) {
		return ErrInvalidSlug
	}
	res, err := w.db.ExecContext(ctx, `UPDATE workspaces SET slug = ? WHERE id = ?`, slug, w.id)
	if isSlugTaken(err) {
		return ErrSlugTaken
	}
	if err != nil {
		return err
	}
	return requireRow(res)
}

// Rename changes the workspace's name.
func (w *WS) Rename(ctx context.Context, name string) error {
	res, err := w.db.ExecContext(ctx, `UPDATE workspaces SET name = ? WHERE id = ?`, name, w.id)
	if err != nil {
		return err
	}
	return requireRow(res)
}

// CountTargets returns the number of targets in the workspace.
func (w *WS) CountTargets(ctx context.Context) (int, error) {
	var n int
	err := w.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM targets WHERE workspace_id = ?`, w.id).Scan(&n)
	return n, err
}

// CountWorkspaces returns the number of workspaces.
func (s *Store) CountWorkspaces(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workspaces`).Scan(&n)
	return n, err
}

// TargetsInOtherWorkspaces returns the targets of every workspace except
// exceptWS, any workspace that has an operator as a member and any suspended
// workspace, for the host-rate check. Only ID, WorkspaceID, URL and Interval are filled in.
func (s *Store) TargetsInOtherWorkspaces(ctx context.Context, exceptWS int64) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.id, t.workspace_id, t.url, t.interval_ms FROM targets t`+activeWorkspace+`
		 WHERE t.workspace_id <> ? AND t.workspace_id <> ?
		   AND t.workspace_id NOT IN (SELECT m.workspace_id FROM memberships m JOIN users u ON u.id = m.user_id WHERE u.is_operator <> 0)
		 ORDER BY t.id`,
		exceptWS, OperatorWorkspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Target
	for rows.Next() {
		var t Target
		var ms int64
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.URL, &ms); err != nil {
			return nil, err
		}
		t.Interval = time.Duration(ms) * time.Millisecond
		out = append(out, t)
	}
	return out, rows.Err()
}

// inWS restricts a query on a table with a target_id column (checks,
// incidents, alerts, daily) to the targets of one workspace. It takes one
// argument: the workspace id.
const inWS = `target_id IN (SELECT id FROM targets WHERE workspace_id = ?)`

// Workspace is an isolated tenant: it owns targets, maintenance windows and
// settings.
type Workspace struct {
	ID        int64
	Name      string
	Slug      string
	CreatedAt time.Time
}

// WS is a workspace-bound view of the store. Every query it runs is
// constrained to one workspace, so the web layer can only ever see its own
// account's data. A query here without a workspace filter is a security bug.
type WS struct {
	db *sql.DB
	id int64
}

// Workspace returns the view of workspace id. It panics on a non-positive id,
// which can only be a programming error.
func (s *Store) Workspace(id int64) *WS {
	if id <= 0 {
		panic("store: invalid workspace id")
	}
	return &WS{db: s.db, id: id}
}

// ID returns the workspace id.
func (w *WS) ID() int64 { return w.id }

// scope returns the SQL condition and arguments limiting a target_id column
// to this workspace and, when targetID is not 0, to that one target.
func (w *WS) scope(targetID int64) (string, []any) {
	if targetID == 0 {
		return inWS, []any{w.id}
	}
	return inWS + ` AND target_id = ?`, []any{w.id, targetID}
}

const workspaceCols = `id, name, slug, created_at`

func scanWorkspace(sc scanner) (Workspace, error) {
	var ws Workspace
	var created int64
	if err := sc.Scan(&ws.ID, &ws.Name, &ws.Slug, &created); err != nil {
		return Workspace{}, err
	}
	ws.CreatedAt = time.UnixMilli(created).UTC()
	return ws, nil
}

// Info returns the workspace's own record.
func (w *WS) Info(ctx context.Context) (Workspace, error) {
	ws, err := scanWorkspace(w.db.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE id = ?`, w.id))
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	return ws, err
}

// WorkspaceBySlug looks a workspace up by its slug, or returns ErrNotFound. A
// workspace none of whose owners is verified is not found (an unverified
// account's status page is 404); the operator workspace is always found.
func (s *Store) WorkspaceBySlug(ctx context.Context, slug string) (Workspace, error) {
	ws, err := scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT `+workspaceCols+` FROM workspaces WHERE slug = ? AND suspended_at IS NULL AND (id = ? OR EXISTS (
		   SELECT 1 FROM memberships m JOIN users u ON u.id = m.user_id
		   WHERE m.workspace_id = workspaces.id AND m.role = 'owner' AND u.verified_at IS NOT NULL))`,
		slug, OperatorWorkspaceID))
	if errors.Is(err, sql.ErrNoRows) {
		return Workspace{}, ErrNotFound
	}
	return ws, err
}

// createWorkspace inserts a workspace. It takes a querier so account creation
// can run it inside its transaction.
func createWorkspace(ctx context.Context, q querier, name, slug string, now time.Time) (Workspace, error) {
	res, err := q.ExecContext(ctx,
		`INSERT INTO workspaces (name, slug, created_at) VALUES (?, ?, ?)`, name, slug, now.UnixMilli())
	if isSlugTaken(err) {
		return Workspace{}, ErrSlugTaken
	}
	if err != nil {
		return Workspace{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: id, Name: name, Slug: slug, CreatedAt: now.UTC()}, nil
}
