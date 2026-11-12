package web

import (
	"context"
	"database/sql"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"uptimemon/internal/store"
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
	// Each operator POST route must answer an operator's cross-site POST with 403.
	posts := 0
	for _, r := range e.srv.routeTable() {
		if r.class == classOperator && r.method == "POST" {
			posts++
			p := req{"POST", strings.ReplaceAll(r.pattern, "{id}", "1")}
			if resp := do(e.op, p, true); resp.StatusCode != http.StatusForbidden {
				t.Errorf("operator cross-site POST %s: %d, want 403", p.path, resp.StatusCode)
			}
		}
	}
	if posts == 0 {
		t.Fatal("no operator POST routes in the table")
	}

	// Path tricks: the mux may redirect a cleaned path, but never to a place
	// that answers anything but 404 to a non-operator.
	tricks := []req{{"HEAD", "/admin/users"}, {"OPTIONS", "/admin/users"}, {"GET", "/%61dmin"},
		{"GET", "/%61dmin/users"}, {"GET", "/admin/.."}, {"GET", "//admin"}, {"GET", "//admin/users"},
		{"GET", "/admin/./users"}, {"GET", "/admin/../admin/users"}}
	for name, c := range map[string]*http.Client{"anonymous": e.client(t), "member": member} {
		for _, r := range tricks {
			resp := do(c, r, false)
			switch {
			case resp.StatusCode == http.StatusNotFound:
			case resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusTemporaryRedirect || resp.StatusCode == http.StatusPermanentRedirect:
				loc, err := url.Parse(resp.Header.Get("Location"))
				if err != nil {
					t.Fatal(err)
				}
				if loc.Path != "/admin" && !strings.HasPrefix(loc.Path, "/admin/") {
					continue // left the panel; gated like any other page
				}
				if resp := do(c, req{r.method, loc.RequestURI()}, false); resp.StatusCode != http.StatusNotFound {
					t.Errorf("%s %s %s -> %s: status %d, want 404", name, r.method, r.path, loc, resp.StatusCode)
				}
			case r.path == "/admin/.." && resp.StatusCode == http.StatusSeeOther: // cleaned to / and sent to sign in
			default:
				t.Errorf("%s %s %s: status %d, want 404 or a gated redirect", name, r.method, r.path, resp.StatusCode)
			}
		}
	}
}

func TestAdminDisabledOperatorGets404(t *testing.T) {
	e := newEnv(t)
	phc, err := e.hash.Hash(context.Background(), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := e.st.CreateAccount(context.Background(), store.AccountInput{Email: "op2@example.com", PasswordHash: phc,
		WorkspaceName: "op2", Slug: "op2", Verified: true, Operator: true, Now: e.now()})
	if err != nil {
		t.Fatal(err)
	}
	c := e.signIn(t, "op2@example.com", testSecret)
	if resp, _ := e.get(t, c, "/admin/users"); resp.StatusCode != http.StatusOK {
		t.Fatalf("second operator before disable: %d", resp.StatusCode)
	}
	// SetUserDisabled refuses operators, so flip the column directly while the session survives.
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, e.now().UnixMilli(), u.ID); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/admin", "/admin/users"} {
		if resp, _ := e.get(t, c, p); resp.StatusCode != http.StatusNotFound {
			t.Errorf("disabled operator GET %s: %d, want 404", p, resp.StatusCode)
		}
	}
	if resp, _ := e.post(t, c, "/admin/users/1/disable", url.Values{}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("disabled operator POST: %d, want 404", resp.StatusCode)
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
	member, _ := e.account(t, "member@example.com")
	req := httptest.NewRequest("GET", e.ts.URL+"/admin", nil)
	for _, ck := range member.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("signed-in non-operator: %d, want 404", rec.Code)
	}
	req = httptest.NewRequest("GET", e.ts.URL+"/admin", nil)
	e.authorize(req)
	rec = httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Errorf("operator: %d, want the handler's 418", rec.Code)
	}
}

// ---- users page ----

func (e *env) userID(t *testing.T, email string) int64 {
	t.Helper()
	u, _, err := e.st.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func (e *env) userExists(t *testing.T, email string) bool {
	t.Helper()
	_, _, err := e.st.UserByEmail(context.Background(), email)
	return err == nil
}

// upstream is a monitored endpoint that counts the probes it receives.
func upstream(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	t.Cleanup(ts.Close)
	return ts, &n
}

// monitor adds a running 1s monitor for url to ws.
func (e *env) monitor(t *testing.T, ws *store.WS, name, url string) store.Target {
	t.Helper()
	tg, err := ws.CreateTarget(t.Context(), store.Target{Name: name, URL: url, Interval: time.Second, Timeout: time.Second}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
		t.Fatal(err)
	}
	return tg
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(6 * time.Second); !ok(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// follow GETs the Location of a 303 as the operator and returns the body.
func (e *env) follow(t *testing.T, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	_, body := e.get(t, e.op, resp.Header.Get("Location"))
	return body
}

func (e *env) adminPost(t *testing.T, path string, extra url.Values) *http.Response {
	t.Helper()
	resp, _ := e.post(t, e.op, path, extra)
	return resp
}

func TestAdminUsersList(t *testing.T) {
	e := newEnv(t)
	up, _ := upstream(t)
	_, bws := e.account(t, "bob@example.com")
	e.account(t, "carol@example.com")
	e.monitor(t, bws, "B1", up.URL+"/needle")
	e.monitor(t, bws, "B2", up.URL)
	_, body := e.get(t, e.op, "/admin/users")
	for _, want := range []string{opEmail, "bob@example.com", "carol@example.com", `href="/s/bob"`, "operator"} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	if !strings.Contains(body, "<td>2</td>") {
		t.Errorf("monitor count 2 missing:\n%s", body)
	}
	_, body = e.get(t, e.op, "/admin/users?q=bob")
	if !strings.Contains(body, "bob@example.com") || strings.Contains(body, "carol@example.com") {
		t.Errorf("q filter: %s", body)
	}
	_, body = e.get(t, e.op, "/admin/users?host="+url.QueryEscape("/needle"))
	if !strings.Contains(body, "bob@example.com") || strings.Contains(body, "carol@example.com") {
		t.Errorf("host filter: %s", body)
	}
	if resp, _ := e.get(t, e.op, "/admin/users?page=abc"); resp.StatusCode != http.StatusOK {
		t.Errorf("bad page: %d", resp.StatusCode)
	}
}

func TestAdminDisableEnable(t *testing.T) {
	e := newEnv(t)
	up, n := upstream(t)
	bc, bws := e.account(t, "bob@example.com")
	e.monitor(t, bws, "B", up.URL)
	waitFor(t, "first probe", func() bool { return n.Load() > 0 })
	id := strconv.FormatInt(e.userID(t, "bob@example.com"), 10)
	keep := url.Values{"q": {"bob"}, "host": {"x"}, "page": {"1"}}

	resp := e.adminPost(t, "/admin/users/"+id+"/disable", keep)
	loc := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/admin/users?") || !strings.Contains(loc, "q=bob") || !strings.Contains(loc, "host=x") {
		t.Fatalf("disable: %d %q", resp.StatusCode, loc)
	}
	if body := e.follow(t, resp); !strings.Contains(body, "User disabled.") {
		t.Errorf("flash missing: %s", body)
	}
	if r, _ := e.get(t, bc, "/"); r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Errorf("disabled user's cookie reached /: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get(t, e.client(t), "/s/bob"); r.StatusCode != http.StatusNotFound {
		t.Errorf("status page of disabled user: %d", r.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	before := n.Load()
	e.advance(10 * time.Minute)
	time.Sleep(1500 * time.Millisecond)
	if after := n.Load(); after != before {
		t.Errorf("monitor kept probing while disabled: %d -> %d", before, after)
	}

	resp = e.adminPost(t, "/admin/users/"+id+"/enable", keep)
	if body := e.follow(t, resp); !strings.Contains(body, "User enabled.") {
		t.Errorf("flash missing: %s", body)
	}
	bc = e.signIn(t, "bob@example.com", testSecret)
	if r, _ := e.get(t, bc, "/"); r.StatusCode != http.StatusOK {
		t.Errorf("enabled user's dashboard: %d", r.StatusCode)
	}
	if r, _ := e.get(t, e.client(t), "/s/bob"); r.StatusCode != http.StatusOK {
		t.Errorf("status page after enable: %d", r.StatusCode)
	}
	before = n.Load()
	waitFor(t, "probes after enable", func() bool { return n.Load() > before })
	if r, _ := e.post(t, e.op, "/admin/users/99999/disable", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown user: %d, want 404", r.StatusCode)
	}
}

func TestAdminCannotDisableOperator(t *testing.T) {
	e := newEnv(t)
	phc, _ := e.hash.Hash(context.Background(), testSecret)
	op2, _, err := e.st.CreateAccount(context.Background(), store.AccountInput{Email: "op2@example.com", PasswordHash: phc,
		WorkspaceName: "op2", Slug: "op2", Verified: true, Operator: true, Now: e.now()})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{e.userID(t, opEmail), op2.ID} {
		resp := e.adminPost(t, "/admin/users/"+strconv.FormatInt(id, 10)+"/disable", url.Values{})
		if body := e.follow(t, resp); !strings.Contains(body, "Operators cannot be disabled.") {
			t.Errorf("refusal flash for %d missing: %s", id, body)
		}
		u, err := e.st.UserByID(context.Background(), id)
		if err != nil || u.DisabledAt != nil {
			t.Errorf("operator %d changed: %+v %v", id, u, err)
		}
	}
	if r, _ := e.get(t, e.op, "/admin/users"); r.StatusCode != http.StatusOK {
		t.Errorf("operator locked out: %d", r.StatusCode)
	}
}

func TestAdminDelete(t *testing.T) {
	e := newEnv(t)
	up, n := upstream(t)
	_, bws := e.account(t, "bob@example.com")
	tg := e.monitor(t, bws, "B", up.URL)
	waitFor(t, "first probe", func() bool { return n.Load() > 0 })
	id := strconv.FormatInt(e.userID(t, "bob@example.com"), 10)

	if r, _ := e.get(t, e.client(t), "/s/bob"); r.StatusCode != http.StatusOK {
		t.Fatalf("anonymous status page: %d", r.StatusCode)
	}
	cached := func() bool {
		e.srv.statusCache.mu.Lock()
		defer e.srv.statusCache.mu.Unlock()
		for k := range e.srv.statusCache.entries {
			if wsOf(k) == bws.ID() {
				return true
			}
		}
		return false
	}
	if !cached() {
		t.Fatal("no status cache entry before delete")
	}
	_, list := e.get(t, e.op, "/admin/users?q=bob&host=")
	m := regexp.MustCompile(`href="(/admin/users/` + id + `/delete[^"]*)"`).FindStringSubmatch(list)
	if m == nil {
		t.Fatalf("no delete link on the filtered list: %s", list)
	}
	resp, body := e.get(t, e.op, html.UnescapeString(m[1]))
	if !strings.Contains(body, `name="q" value="bob"`) {
		t.Errorf("confirmation page lost the filter: %s", body)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "bob@example.com") || !strings.Contains(body, `type="password"`) {
		t.Fatalf("confirmation page: %d %s", resp.StatusCode, body)
	}
	resp, body = e.post(t, e.op, "/admin/users/"+id+"/delete", url.Values{"current": {"wrong"}, "q": {"bob"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgBadCurrent) || !strings.Contains(body, `type="password"`) {
		t.Fatalf("wrong password: %d %s", resp.StatusCode, body)
	}
	if !e.userExists(t, "bob@example.com") {
		t.Fatal("user deleted by a wrong password")
	}
	resp = e.adminPost(t, "/admin/users/"+id+"/delete", url.Values{"current": {testSecret}, "q": {"bob"}})
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/admin/users?") || !strings.Contains(loc, "q=bob") {
		t.Errorf("redirect %q", loc)
	}
	if body := e.follow(t, resp); !strings.Contains(body, "User deleted.") {
		t.Errorf("flash missing: %s", body)
	}
	if e.userExists(t, "bob@example.com") {
		t.Error("user survived")
	}
	if _, err := e.st.Workspace(bws.ID()).Info(context.Background()); err == nil {
		t.Error("workspace survived")
	}
	if _, ok := e.mon.Get(bws.ID(), tg.ID); ok {
		t.Error("runner survived")
	}
	if cached() {
		t.Error("status cache entry survived")
	}

	opID := strconv.FormatInt(e.userID(t, opEmail), 10)
	resp = e.adminPost(t, "/admin/users/"+opID+"/delete", url.Values{"current": {testSecret}})
	if body := e.follow(t, resp); !strings.Contains(body, "Operators cannot be deleted.") {
		t.Errorf("operator delete refusal: %s", body)
	}
	if r, _ := e.get(t, e.op, "/admin/users/"+opID+"/delete"); r.StatusCode != http.StatusSeeOther {
		t.Errorf("operator confirmation page: %d, want 303", r.StatusCode)
	}
	if !e.userExists(t, opEmail) {
		t.Error("operator deleted")
	}
}

func TestAdminDeleteFailureLeavesUserDisabled(t *testing.T) {
	e := newEnv(t)
	var buf syncBuf
	e.srv.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	e.account(t, "bob@example.com")
	id := e.userID(t, "bob@example.com")
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER no_user_delete BEFORE DELETE ON users BEGIN SELECT RAISE(ABORT, 'blocked'); END`); err != nil {
		t.Fatal(err)
	}
	resp := e.adminPost(t, "/admin/users/"+strconv.FormatInt(id, 10)+"/delete", url.Values{"current": {testSecret}})
	if body := e.follow(t, resp); !strings.Contains(body, "Delete failed; the user is left disabled.") {
		t.Errorf("flash: %s", body)
	}
	u, err := e.st.UserByID(context.Background(), id)
	if err != nil || u.DisabledAt == nil {
		t.Errorf("user after failed delete: %+v %v", u, err)
	}
	if out := buf.String(); !strings.Contains(out, `"result":"failed"`) || !strings.Contains(out, `"ip"`) {
		t.Errorf("failure log: %s", out)
	}
}

func TestAdminBadUserID(t *testing.T) {
	e := newEnv(t)
	if r := e.adminPost(t, "/admin/users/abc/disable", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("non-numeric id: %d, want 404", r.StatusCode)
	}
}

func TestAdminResetLinkWithoutMail(t *testing.T) {
	e := newEnv(t)
	e.account(t, "bob@example.com")
	id := strconv.FormatInt(e.userID(t, "bob@example.com"), 10)
	resp, body := e.post(t, e.op, "/admin/users/"+id+"/reset-link", url.Values{"q": {"bob"}})
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("reset link: %d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(body, "data-no-refresh") || !strings.Contains(body, "/reset?token=") {
		t.Fatalf("one-time box missing: %s", body)
	}
	m := linkFrom(body)
	if m == "" {
		t.Fatalf("no link in %s", body)
	}
	anon := e.client(t)
	if r, b := e.get(t, anon, linkPath(m)); r.StatusCode != http.StatusOK || !strings.Contains(b, "password") {
		t.Errorf("link does not work: %d %s", r.StatusCode, b)
	}
	if _, body := e.get(t, e.op, "/admin/users"); strings.Contains(body, "/reset?token=") {
		t.Error("link shown again on a fresh GET")
	}
	opID := strconv.FormatInt(e.userID(t, opEmail), 10)
	resp, body = e.post(t, e.op, "/admin/users/"+opID+"/reset-link", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(body, "/reset?token=") {
		t.Errorf("operator link: %d", resp.StatusCode)
	}
	if b := e.follow(t, resp); !strings.Contains(b, "Operators cannot be sent a reset link.") {
		t.Errorf("refusal flash: %s", b)
	}
	// A disabled user gets none either.
	if _, err := e.st.SetUserDisabled(context.Background(), e.userID(t, "bob@example.com"), true, e.now()); err != nil {
		t.Fatal(err)
	}
	resp, body = e.post(t, e.op, "/admin/users/"+id+"/reset-link", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(body, "/reset?token=") {
		t.Errorf("disabled user link: %d", resp.StatusCode)
	}
}

func TestAdminResetLinkWithMail(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "bob@example.com")
	id := strconv.FormatInt(e.userID(t, "bob@example.com"), 10)
	resp, body := e.post(t, e.op, "/admin/users/"+id+"/reset-link", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(body, "/reset?token=") {
		t.Fatalf("with mail: %d %s", resp.StatusCode, body)
	}
	if b := e.follow(t, resp); !strings.Contains(b, "Reset email queued (subject to mail limits).") || strings.Contains(b, "/reset?token=") {
		t.Errorf("flash: %s", b)
	}
	sent := e.mail.all()
	if len(sent) != 1 || sent[0].To != "bob@example.com" || !strings.Contains(sent[0].Body, "/reset?token=") {
		t.Fatalf("mail: %+v", sent)
	}
}

func TestAdminResendVerification(t *testing.T) {
	e := newEnvMail(t, openCfg)
	phc, _ := e.hash.Hash(context.Background(), testSecret)
	if _, _, err := e.st.CreateAccount(context.Background(), store.AccountInput{Email: "new@example.com", PasswordHash: phc,
		WorkspaceName: "new", Slug: "new", Now: e.now()}); err != nil {
		t.Fatal(err)
	}
	e.account(t, "bob@example.com")
	resp := e.adminPost(t, "/admin/users/"+strconv.FormatInt(e.userID(t, "new@example.com"), 10)+"/resend", url.Values{})
	if body := e.follow(t, resp); !strings.Contains(body, "Verification email queued (subject to mail limits).") {
		t.Errorf("flash: %s", body)
	}
	sent := e.mail.all()
	if len(sent) != 1 || sent[0].To != "new@example.com" || !strings.Contains(sent[0].Body, "/verify?token=") {
		t.Fatalf("mail: %+v", sent)
	}
	if r := e.adminPost(t, "/admin/users/"+strconv.FormatInt(e.userID(t, "bob@example.com"), 10)+"/resend", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("verified user: %d, want 404", r.StatusCode)
	}
	if len(e.mail.all()) != 1 {
		t.Error("mail sent for a verified user")
	}
}

func TestAdminResendNeedsMail(t *testing.T) {
	e := newEnv(t)
	phc, _ := e.hash.Hash(context.Background(), testSecret)
	u, _, err := e.st.CreateAccount(context.Background(), store.AccountInput{Email: "new@example.com", PasswordHash: phc,
		WorkspaceName: "new", Slug: "new", Now: e.now()})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.adminPost(t, "/admin/users/"+strconv.FormatInt(u.ID, 10)+"/resend", url.Values{}); r.StatusCode != http.StatusNotFound {
		t.Errorf("no mail: %d, want 404", r.StatusCode)
	}
}

func TestOperatorActionLogged(t *testing.T) {
	e := newEnv(t)
	var buf syncBuf
	e.srv.logger = slog.New(slog.NewJSONHandler(&buf, nil))
	e.account(t, "bob@example.com")
	id := e.userID(t, "bob@example.com")
	e.adminPost(t, "/admin/users/"+strconv.FormatInt(id, 10)+"/disable", url.Values{})
	out := buf.String()
	for _, want := range []string{`"event":"operator_action"`, `"action":"disable"`, `"ip":`, `"user_id":` + strconv.FormatInt(e.userID(t, opEmail), 10),
		`"target_user_id":` + strconv.FormatInt(id, 10)} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %s:\n%s", want, out)
		}
	}
	var buf2 syncBuf
	e.srv.logger = slog.New(slog.NewJSONHandler(&buf2, nil))
	e.adminPost(t, "/admin/users/"+strconv.FormatInt(e.userID(t, opEmail), 10)+"/disable", url.Values{})
	if out := buf2.String(); !strings.Contains(out, `"result":"refused"`) {
		t.Errorf("refusal not logged: %s", out)
	}
}
