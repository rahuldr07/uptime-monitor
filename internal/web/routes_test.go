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
	e := newEnv(t)
	routes := testRoutes(t, e)
	if len(routes) != 42 {
		t.Errorf("len = %d, want 42", len(routes))
	}
	seen := map[string]bool{}
	byClass := map[routeClass]map[string]bool{}
	for _, r := range routes {
		key := r.method + " " + r.pattern
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true
		if r.method != "GET" && r.method != "POST" {
			t.Errorf("%s: invalid method", key)
		}
		switch r.class {
		case classPublic, classSetup, classGuest:
			if r.handler == nil || r.authed != nil {
				t.Errorf("%s: class %d needs handler only", key, r.class)
			}
		case classUser, classWorkspace:
			if r.authed == nil || r.handler != nil {
				t.Errorf("%s: class %d needs authed only", key, r.class)
			}
		default:
			t.Errorf("%s: invalid class %d", key, r.class)
		}
		if byClass[r.class] == nil {
			byClass[r.class] = map[string]bool{}
		}
		byClass[r.class][key] = true
	}
	for class, want := range map[routeClass][]string{
		classPublic: {"GET /static/", "GET /healthz", "GET /status", "GET /s/{slug}"},
		classSetup:  {"GET /setup", "POST /setup"},
		classGuest:  {"GET /login", "POST /login", "GET /signup", "POST /signup", "GET /reset", "POST /reset"},
		classUser:   {"POST /logout", "GET /account", "POST /account/password", "POST /account/signout-all"},
	} {
		if len(byClass[class]) != len(want) {
			t.Errorf("class %d routes = %v, want %v", class, byClass[class], want)
		}
		for _, k := range want {
			if !byClass[class][k] {
				t.Errorf("%s is not class %d", k, class)
			}
		}
	}
}

func TestRouteTableDefaultDeny(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	for _, r := range testRoutes(t, e) {
		path := strings.ReplaceAll(strings.Replace(r.pattern, "/{$}", "/", 1), "{id}", "1")
		if r.class == classWorkspace || r.class == classUser {
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
				t.Errorf("POST %s cross-site: status %d, want 423", path, resp.StatusCode)
			}
		}
	}
}
