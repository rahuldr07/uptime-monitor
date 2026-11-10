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

// sysAllowed lists the non-test files that may use the unscoped store (s.sys).
// Everything else must reach workspace data through the signed-in user's *store.WS.
var sysAllowed = map[string]string{
	"session.go": "resolves a session cookie to a user",
	"setup.go":   "first-run setup",
	"login.go":   "sign-in",
	"account.go": "password and session management for the signed-in user",
	"reset.go":   "password reset",
	"web.go":     "healthz Ping",
	"status.go":  "public status page slug resolver",
	"events.go":  "session re-check for open streams",
}

func TestSysAllowList(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	used := 0
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
		ast.Inspect(file, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "sys" {
				used++
				if _, ok := sysAllowed[f]; !ok {
					t.Errorf("%s: %s uses the unscoped store (.sys); use the request's workspace", f, fset.Position(sel.Pos()))
				}
			}
			return true
		})
	}
	if used == 0 {
		t.Fatal("found no .sys uses at all; the check is not looking at the right files")
	}
}

func TestEveryRouteClassified(t *testing.T) {
	const want = 40
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
