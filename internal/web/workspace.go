package web

import (
	"net/http"

	"uptimemon/internal/store"
)

// pinnedWorkspace is the only workspace the web layer serves until sign-in
// lands.
const pinnedWorkspace int64 = 1

// ws returns the workspace a request operates on. It is pinned to workspace 1;
// M1d replaces it with the signed-in user's workspace.
func (s *Server) ws(r *http.Request) *store.WS { return s.sys.Workspace(pinnedWorkspace) }
