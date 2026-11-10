package web

import (
	"net/http"
	"strings"
	"testing"
)

func testRoutes(t *testing.T, e *env) []route {
	t.Helper()
	return e.srv.routeTable()
}

func TestRouteTableIsComplete(t *testing.T) {
	e := newEnv(t, "pw")
	routes := testRoutes(t, e)
	if len(routes) != 32 {
		t.Errorf("len = %d, want 32", len(routes))
	}
	seen := map[string]bool{}
	public := map[string]bool{}
	for _, r := range routes {
		key := r.method + " " + r.pattern
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true
		if r.class != classPublic && r.class != classAdmin {
			t.Errorf("%s: invalid class %d", key, r.class)
		}
		if r.method != "GET" && r.method != "POST" {
			t.Errorf("%s: invalid method", key)
		}
		if r.handler == nil {
			t.Errorf("%s: nil handler", key)
		}
		if r.class == classPublic {
			public[key] = true
		}
	}
	want := []string{"GET /static/", "GET /healthz", "GET /status", "GET /login", "POST /login", "POST /logout"}
	if len(public) != len(want) {
		t.Errorf("public routes = %v, want %v", public, want)
	}
	for _, k := range want {
		if !public[k] {
			t.Errorf("%s is not public", k)
		}
	}
}

func TestRouteTableDefaultDeny(t *testing.T) {
	e := newEnv(t, "pw")
	c := e.client(t)
	for _, r := range testRoutes(t, e) {
		path := strings.ReplaceAll(strings.Replace(r.pattern, "/{$}", "/", 1), "{id}", "1")
		if r.class == classAdmin {
			req, _ := http.NewRequest(r.method, e.ts.URL+path, nil)
			if r.method == "POST" {
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			}
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			ok := resp.StatusCode == http.StatusUnauthorized ||
				(resp.StatusCode == http.StatusSeeOther && strings.HasPrefix(resp.Header.Get("Location"), "/login"))
			if !ok {
				t.Errorf("%s %s anonymous: status %d, location %q", r.method, path, resp.StatusCode, resp.Header.Get("Location"))
			}
		}
		if r.method == "POST" {
			req, _ := http.NewRequest("POST", e.ts.URL+path, nil)
			req.Header.Set("Origin", "https://evil.example")
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("POST %s cross-site: status %d, want 403", path, resp.StatusCode)
			}
		}
	}
}
