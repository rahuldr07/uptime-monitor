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
	if len(routes) != 80 {
		t.Errorf("len = %d, want 80", len(routes))
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
		case classUser, classWorkspace, classOperator:
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
		classPublic: {"GET /", "GET /static/", "GET /healthz", "GET /status", "GET /s/{slug}", "GET /verify", "POST /verify", "POST /verify/resend", "GET /reset", "POST /reset", "GET /auth/{provider}/start", "GET /auth/{provider}/callback"},
		classSetup:  {"GET /setup", "POST /setup"},
		classGuest:  {"GET /login", "POST /login", "GET /signup", "POST /signup", "GET /forgot", "POST /forgot"},
		classUser: {"POST /logout", "GET /settings", "GET /settings/profile", "POST /settings/profile/timezone", "POST /settings/tz-detected", "GET /settings/security",
			"POST /settings/security/password", "POST /settings/security/signout-all", "POST /settings/security/identities/{provider}/delete", "POST /settings/security/identities/{provider}/connect",
			"GET /settings/danger", "POST /settings/danger/delete",
			"GET /account", "POST /account/password", "POST /account/signout-all", "POST /account/delete", "POST /account/identities/{provider}/delete", "POST /account/identities/{provider}/connect", "POST /account/timezone", "POST /account/tz-detected"},
		classOperator: {"GET /admin", "GET /admin/users", "GET /admin/users/{id}/delete", "POST /admin/users/{id}/disable",
			"POST /admin/users/{id}/enable", "POST /admin/users/{id}/delete", "POST /admin/users/{id}/resend", "POST /admin/users/{id}/reset-link",
			"GET /admin/invites", "POST /admin/invites", "POST /admin/invites/revoke"},
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
		if r.class == classOperator {
			req, _ := http.NewRequest(r.method, e.ts.URL+path, nil)
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s %s anonymous: status %d, want 404", r.method, path, resp.StatusCode)
			}
		}
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
			want := http.StatusForbidden
			if r.class == classOperator {
				want = http.StatusNotFound
			}
			if resp.StatusCode != want {
				t.Errorf("POST %s cross-site: status %d, want %d", path, resp.StatusCode, want)
			}
		}
	}
}
