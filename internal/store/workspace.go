package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

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

// WorkspaceBySlug looks a workspace up by its slug, or returns ErrNotFound.
func (s *Store) WorkspaceBySlug(ctx context.Context, slug string) (Workspace, error) {
	ws, err := scanWorkspace(s.db.QueryRowContext(ctx, `SELECT `+workspaceCols+` FROM workspaces WHERE slug = ?`, slug))
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
	if err != nil {
		return Workspace{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Workspace{}, err
	}
	return Workspace{ID: id, Name: name, Slug: slug, CreatedAt: now.UTC()}, nil
}
