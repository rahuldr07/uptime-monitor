package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sysAllowed lists, per file and enclosing function, the methods of the
// unscoped store (s.sys) that may be called. Everything else, including the
// workspace handlers that live in the same files, must reach data through the
// signed-in user's *store.WS.
var sysAllowed = map[string][]string{
	"session.go|(*Server).lookupAuth":        {"SessionAuth"},            // session cookie -> user
	"setup.go|(*Server).setupPage":           {"Workspace", "PeekToken"}, // first-run setup
	"setup.go|(*Server).setupPost":           {"PeekToken", "CompleteSetup", "CreateSession"},
	"signup.go|(*Server).signupPage":         {"PeekToken"},
	"signup.go|(*Server).signupPost":         {"PeekToken", "CreateAccount", "CreateSession"},
	"login.go|(*Server).loginPost":           {"UserByEmail", "DeleteSession", "CreateSession"},
	"login.go|(*Server).logout":              {"DeleteSession"},
	"account.go|(*Server).accountPassword":   {"PasswordHash", "SetPassword"},
	"account.go|(*Server).accountSignOutAll": {"DeleteUserSessions"},
	"reset.go|(*Server).resetPage":           {"PeekToken"},
	"reset.go|(*Server).resetPost":           {"PeekToken", "ResetPassword", "CreateSession"},
	"targets.go|(*Server).hostRateErr":       {"TargetsInOtherWorkspaces"}, // combined per-host rate across workspaces
	"web.go|(*Server).healthz":               {"Ping"},
	"status.go|(*Server).statusRedirect":     {"Workspace"},                    // operator's page for anonymous visitors
	"status.go|(*Server).statusPage":         {"WorkspaceBySlug", "Workspace"}, // slug resolver and anonymous page build
	"events.go|(*Server).events":             {"SessionAuth"},                  // session re-check only
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	switch rt := fd.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := rt.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fd.Name.Name
		}
	case *ast.Ident:
		return rt.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}

func TestSysAllowList(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	used := map[string]bool{} // "file|func|Method" seen
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				// Package-level declarations (vars, closures) have no function to allow.
				ast.Inspect(d, func(n ast.Node) bool {
					if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "sys" {
						t.Errorf("%s: %s uses the unscoped store (.sys) outside a function", f, fset.Position(sel.Pos()))
					}
					return true
				})
				continue
			}
			key := f + "|" + funcName(fd)
			allowed := map[string]bool{}
			for _, m := range sysAllowed[key] {
				allowed[m] = true
			}
			covered := map[token.Pos]bool{}
			ast.Inspect(fd, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "sys" {
					covered[inner.Pos()] = true
					used[key+"|"+sel.Sel.Name] = true
					if !allowed[sel.Sel.Name] {
						t.Errorf("%s: %s calls .sys.%s in %s; use the request's workspace", f, fset.Position(sel.Pos()), sel.Sel.Name, funcName(fd))
					}
				} else if sel.Sel.Name == "sys" && !covered[sel.Pos()] {
					t.Errorf("%s: %s uses the unscoped store (.sys) other than by calling a method on it", f, fset.Position(sel.Pos()))
				}
				return true
			})
		}
	}
	// Stale entries would let a future use slip in unnoticed.
	for key, methods := range sysAllowed {
		for _, m := range methods {
			if !used[key+"|"+m] {
				t.Errorf("allow-list entry %s .sys.%s is unused; remove it", key, m)
			}
		}
	}
}

func TestEveryRouteClassified(t *testing.T) {
	const want = 42
	e := newEnv(t)
	routes := e.srv.routeTable()
	if len(routes) != want {
		t.Errorf("route table has %d routes, want %d; classify and add isolation cases for new routes, then update this count", len(routes), want)
	}
	seen := map[string]bool{}
	for _, r := range routes {
		key := r.method + " " + r.pattern
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true
		if r.class < classPublic || r.class > classWorkspace {
			t.Errorf("%s: invalid class %d", key, r.class)
		}
		isAuthed := r.class == classUser || r.class == classWorkspace
		if (r.authed != nil) != isAuthed {
			t.Errorf("%s: authed handler set = %v, but class %d needs %v", key, r.authed != nil, r.class, isAuthed)
		}
		if (r.handler != nil) == isAuthed {
			t.Errorf("%s: plain handler set = %v, but class %d is authed = %v", key, r.handler != nil, r.class, isAuthed)
		}
		if r.method != "GET" && r.method != "POST" {
			t.Errorf("%s: unexpected method", key)
		}
	}
}
