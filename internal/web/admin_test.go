package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminNotFoundForOthers(t *testing.T) {
	e := newEnv(t)
	member, _ := e.account(t, "member@example.com")
	type req struct{ method, path string }
	reqs := []req{{"GET", "/admin/users/1/disable"}, {"GET", "/admin/nope"}, {"GET", "/admin/"},
		{"POST", "/admin/nope"}, {"POST", "/admin"}, {"PUT", "/admin"}}
	for _, r := range e.srv.routeTable() {
		if r.class == classOperator {
			reqs = append(reqs, req{r.method, strings.ReplaceAll(r.pattern, "{id}", "1")})
		}
	}
	if len(reqs) <= 6 {
		t.Fatal("no operator routes in the table")
	}
	do := func(c *http.Client, r req, crossSite bool) *http.Response {
		hr, _ := http.NewRequest(r.method, e.ts.URL+r.path, nil)
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if crossSite {
			hr.Header.Set("Origin", "https://evil.example")
		}
		resp, err := c.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for name, c := range map[string]*http.Client{"anonymous": e.client(t), "member": member} {
		for _, r := range reqs {
			for _, cross := range []bool{false, true} {
				if resp := do(c, r, cross); resp.StatusCode != http.StatusNotFound {
					t.Errorf("%s %s %s cross=%v: status %d, want 404", name, r.method, r.path, cross, resp.StatusCode)
				}
			}
		}
	}
	resp := do(e.op, req{"GET", "/admin"}, false)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/users" {
		t.Errorf("operator GET /admin: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// No operator POST route exists until the panel's actions land; each one
	// added later must answer an operator's cross-site POST with 403.
	for _, r := range e.srv.routeTable() {
		if r.class == classOperator && r.method == "POST" {
			p := req{"POST", strings.ReplaceAll(r.pattern, "{id}", "1")}
			if resp := do(e.op, p, true); resp.StatusCode != http.StatusForbidden {
				t.Errorf("operator cross-site POST %s: %d, want 403", p.path, resp.StatusCode)
			}
		}
	}
}

func TestOperatorSidebarLink(t *testing.T) {
	e := newEnv(t)
	member, _ := e.account(t, "member@example.com")
	if _, body := e.get(t, e.op, "/"); !strings.Contains(body, `href="/admin"`) {
		t.Error("operator sidebar lacks Admin link")
	}
	if _, body := e.get(t, member, "/"); strings.Contains(body, `href="/admin"`) {
		t.Error("member sees Admin link")
	}
}

func TestRequireOperatorSecondLine(t *testing.T) {
	e := newEnv(t)
	h := e.srv.requireOperator(func(w http.ResponseWriter, r *http.Request, a *authCtx) { w.WriteHeader(http.StatusTeapot) })
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("GET", "/admin", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("anonymous: %d, want 404", rec.Code)
	}
}
