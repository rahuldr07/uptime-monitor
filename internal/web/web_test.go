package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/notify"
	"uptimemon/internal/store"
)

type env struct {
	ts    *httptest.Server
	srv   *Server
	st    *store.Store
	mon   *monitor.Monitor
	cfg   *config.Config
	clock time.Time
}

func newEnv(t *testing.T, password string) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, _ := config.Parse([]byte(`{}`))
	cfg.AdminPassword = password
	e := &env{st: st, cfg: cfg, clock: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	now := func() time.Time { return e.clock }
	e.mon = monitor.New(monitor.Options{Store: st, Notifiers: notify.Multi{{Name: "log", Notifier: notify.Log{Logger: slog.Default()}}},
		FailureThreshold: 1, Now: now, AllowPrivate: true})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Config: cfg, Store: st, Monitor: e.mon, Logger: slog.Default(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = s
	e.ts = httptest.NewServer(s.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) client(t *testing.T) *http.Client {
	jar := &cookieJar{}
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(t *testing.T, c *http.Client, path string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(e.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) post(t *testing.T, c *http.Client, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(e.ts.URL+path, form)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestAuthFlow(t *testing.T) {
	e := newEnv(t, "secret")
	c := e.client(t)
	resp, _ := e.get(t, c, "/")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("anon dashboard should redirect to login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := e.get(t, c, "/api/status"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anon api should be 401, got %d", resp.StatusCode)
	}
	if resp, body := e.get(t, c, "/status"); resp.StatusCode != 200 || !strings.Contains(body, "No services configured yet") {
		t.Fatalf("public status page: %d", resp.StatusCode)
	}
	if resp, body := e.post(t, c, "/login", url.Values{"password": {"wrong"}}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Wrong password") {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = e.post(t, c, "/login", url.Values{"password": {"secret"}, "next": {"//evil.com"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login redirect should be sanitised: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := e.get(t, c, "/"); resp.StatusCode != 200 || !strings.Contains(body, "Welcome") {
		t.Fatalf("overview after login: %d", resp.StatusCode)
	}
	for i := 0; i < 6; i++ {
		resp, _ = e.post(t, c, "/login", url.Values{"password": {"x"}})
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected rate limit, got %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, c, "/logout", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, c, "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("should be logged out")
	}
}

func TestTargetLifecycleAndPages(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer up.Close()

	resp, body := e.post(t, c, "/targets", url.Values{"name": {"Site"}, "url": {"ftp://x"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "invalid url") {
		t.Fatalf("validation: %d", resp.StatusCode)
	}
	resp, _ = e.post(t, c, "/targets", url.Values{"name": {"Site"}, "url": {up.URL}, "interval": {"1h"}, "timeout": {"5s"}, "keyword": {"ok"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/targets/") {
		t.Fatalf("create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	loc := strings.SplitN(resp.Header.Get("Location"), "?", 2)[0]
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, body := e.get(t, c, "/api/status"); strings.Contains(body, `"status": "up"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if resp, body := e.get(t, c, loc); resp.StatusCode != 200 || !strings.Contains(body, "<polyline") && !strings.Contains(body, "<circle") {
		t.Fatalf("detail page should include chart: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/"); !strings.Contains(body, "All systems operational") || !strings.Contains(body, "Site") {
		t.Fatal("dashboard should list target as up")
	}
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/", nil)
	req.Header.Set("X-Partial", "1")
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatal("partial request failed")
	} else {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "<html") || !strings.Contains(string(b), "Site") {
			t.Fatal("partial should render content only")
		}
	}
	if _, body := e.get(t, c, loc+"?range=7d"); !strings.Contains(body, `class="active">7d`) {
		t.Fatal("range switch")
	}
	if resp, _ := e.post(t, c, loc, url.Values{"name": {"Site2"}, "url": {up.URL}, "interval": {"2h"}, "timeout": {"5s"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/targets"); !strings.Contains(body, "Site2") || !strings.Contains(body, "<td>2h</td>") {
		t.Fatal("monitors page should show updated target")
	}
	if resp, _ := e.post(t, c, loc+"/pause", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("pause")
	}
	if _, body := e.get(t, c, "/api/status"); !strings.Contains(body, `"status": "paused"`) {
		t.Fatalf("api should show paused: %s", body)
	}
	if _, body := e.get(t, c, "/incidents"); !strings.Contains(body, "No incidents") {
		t.Fatal("incidents page")
	}
	if resp, body := e.get(t, c, "/api/incidents"); resp.StatusCode != 200 || !strings.Contains(body, `"incidents": []`) {
		t.Fatalf("api incidents: %s", body)
	}
	if resp, _ := e.get(t, c, "/api/targets/999/checks"); resp.StatusCode != 404 {
		t.Fatal("unknown target should 404")
	}
	if resp, _ := e.post(t, c, loc+"/delete", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("delete")
	}
	if resp, _ := e.get(t, c, loc); resp.StatusCode != 404 {
		t.Fatal("deleted target should 404")
	}
}

func TestNewEndpoints(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("fine")) }))
	defer up.Close()

	// Test URL probe without saving.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/targets/test", strings.NewReader(url.Values{"url": {up.URL}, "keyword": {"fine"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"ok": true`) || !strings.Contains(string(b), `"status_code": 200`) {
		t.Fatalf("test probe: %s", b)
	}
	if rows, _ := e.st.ListTargets(context.Background()); len(rows) != 0 {
		t.Fatal("test must not save a target")
	}

	// JSON create via the slide-over.
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/targets", strings.NewReader(url.Values{"url": {"example.org/x"}, "name": {"Ex"}, "interval": {"1h"}, "timeout": {"5s"}, "group": {"Core"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, _ = c.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"redirect": "/targets/`) {
		t.Fatalf("json create: %d %s", resp.StatusCode, b)
	}
	tg, _ := e.st.GetTargetByName(context.Background(), "Ex")
	if tg.URL != "https://example.org/x" || tg.Group != "Core" {
		t.Fatalf("scheme should be defaulted and group saved: %+v", tg)
	}
	loc := "/targets/" + strconv.FormatInt(tg.ID, 10)

	// Mute toggles and shows on the page.
	if r, _ := e.post(t, c, loc+"/mute", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatal("mute")
	}
	if _, body := e.get(t, c, loc); !strings.Contains(body, "Unmute alerts") {
		t.Fatal("page should show unmute after muting")
	}
	for _, tab := range []string{"checks", "incidents", "settings"} {
		if r, body := e.get(t, c, loc+"?tab="+tab); r.StatusCode != 200 || !strings.Contains(body, `class="active"`) {
			t.Fatalf("tab %s", tab)
		}
	}

	// Incident ack + note.
	id, _ := e.st.OpenIncident(context.Background(), tg.ID, e.clock.Add(-time.Hour), "HTTP 500")
	if _, body := e.get(t, c, "/incidents"); !strings.Contains(body, "Acknowledge") || !strings.Contains(body, "1 ongoing and unacknowledged") {
		t.Fatal("incidents page should offer acknowledge")
	}
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/incidents/"+strconv.FormatInt(id, 10)+"/ack", nil)
	req.Header.Set("Accept", "application/json")
	resp, _ = c.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"ok": true`) {
		t.Fatalf("ack: %s", b)
	}
	if r, _ := e.post(t, c, "/incidents/"+strconv.FormatInt(id, 10)+"/note", url.Values{"note": {"DB failover"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatal("note")
	}
	if _, body := e.get(t, c, "/incidents"); !strings.Contains(body, "acknowledged") || !strings.Contains(body, `value="DB failover"`) {
		t.Fatal("incidents page should show ack and note")
	}
	if _, body := e.get(t, c, "/api/incidents"); !strings.Contains(body, `"note": "DB failover"`) {
		t.Fatal("api should include note")
	}

	// Status page settings and grouping.
	if r, _ := e.post(t, c, "/status/settings", url.Values{"title": {"Acme status"}, "description": {"All the things"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatal("status settings")
	}
	if _, body := e.get(t, c, "/status"); !strings.Contains(body, "Acme status") || !strings.Contains(body, "All the things") || !strings.Contains(body, `class="group-title">Core`) {
		t.Fatal("status page should show title, description and group")
	}

	// Theme cookie is reflected on <html>.
	req, _ = http.NewRequest(http.MethodGet, e.ts.URL+"/", nil)
	req.AddCookie(&http.Cookie{Name: "theme", Value: "dark"})
	resp, _ = c.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `<html lang="en" data-theme="dark">`) {
		t.Fatal("theme cookie should set data-theme")
	}

	// SSE stream delivers an event when a check runs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+"/events", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %s", resp.Header.Get("Content-Type"))
	}
	e.mon.CheckAsync(tg.ID)
	buf := make([]byte, 4096)
	var got string
	for !strings.Contains(got, "data:") {
		n, err := resp.Body.Read(buf)
		got += string(buf[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(got, `"target_id":`+strconv.FormatInt(tg.ID, 10)) {
		t.Fatalf("expected event for target, got %q", got)
	}
}

func TestAlertsSettings(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	if resp, body := e.get(t, c, "/alerts"); resp.StatusCode != 200 || !strings.Contains(body, "Telegram") {
		t.Fatal("alerts page")
	}
	resp, body := e.post(t, c, "/alerts", url.Values{"slack_url": {"http://insecure"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "https webhook") {
		t.Fatalf("validation: %d", resp.StatusCode)
	}
	resp, _ = e.post(t, c, "/alerts", url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}, "slack_url": {"https://hooks.example/x"},
		"smtp_host": {"h"}, "smtp_from": {"a@b"}, "smtp_to": {"x@y, z@w"}, "email_enabled": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	names := []string{}
	for _, n := range e.mon.Notifiers() {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "telegram,email" {
		t.Fatalf("notifiers after save (slack unticked should be disabled): %v", names)
	}
	if _, body := e.get(t, c, "/alerts"); !strings.Contains(body, "x@y, z@w") || !strings.Contains(body, `value="email"`) {
		t.Fatal("alerts page should show saved settings and test buttons")
	}
	if resp, body := e.post(t, c, "/alerts/test", url.Values{"channel": {"nope"}}); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "failed") {
		t.Fatalf("unknown channel test: %d", resp.StatusCode)
	}
}

func TestHelpers(t *testing.T) {
	if humanDur(90*time.Second) != "1m 30s" || humanDur(26*time.Hour) != "1d 2h" || humanDur(0) != "0s" {
		t.Error("humanDur")
	}
	if shortDur(30*time.Second) != "30s" || shortDur(2*time.Hour) != "2h" || shortDur(90*time.Minute) != "1h30m" || shortDur(10*time.Second) != "10s" {
		t.Errorf("shortDur: %s %s %s", shortDur(30*time.Second), shortDur(2*time.Hour), shortDur(90*time.Minute))
	}
	if humanAgo(time.Second) != "just now" || humanAgo(3*time.Hour) != "3h ago" {
		t.Error("humanAgo")
	}
	if safeNext("https://evil") != "/" || safeNext("/targets/1") != "/targets/1" || safeNext("/login?x") != "/" {
		t.Error("safeNext")
	}
	svg := string(latencySVG(nil, nil, time.Now().Add(-time.Hour), time.Now()))
	if !strings.Contains(svg, "No checks") {
		t.Error("empty chart")
	}
	checks := make([]store.Check, 0, 2000)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 2000; i++ {
		checks = append(checks, store.Check{CheckedAt: base.Add(time.Duration(i) * time.Second), OK: i != 1000, LatencyMS: 100})
	}
	ds := downsample(checks, 600)
	if len(ds) > 700 || len(ds) < 600 {
		t.Errorf("downsample size %d", len(ds))
	}
	found := false
	for _, c := range ds {
		if !c.OK {
			found = true
		}
	}
	if !found {
		t.Error("downsample dropped the failure")
	}
}

// cookieJar is a minimal jar good enough for a single host.
type cookieJar struct{ cookies []*http.Cookie }

func (j *cookieJar) SetCookies(_ *url.URL, cs []*http.Cookie) {
	for _, c := range cs {
		replaced := false
		for i, old := range j.cookies {
			if old.Name == c.Name {
				j.cookies[i] = c
				replaced = true
			}
		}
		if !replaced {
			j.cookies = append(j.cookies, c)
		}
	}
}

func (j *cookieJar) Cookies(_ *url.URL) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range j.cookies {
		if c.MaxAge >= 0 && c.Value != "" {
			out = append(out, c)
		}
	}
	return out
}

func TestSafeNextAndCSRF(t *testing.T) {
	for _, bad := range []string{`/\evil.com`, "//evil.com", "/ evil", "/x\r\nSet-Cookie:a", "https://e", "/login?next=/", "/@evil"} {
		if got := safeNext(bad); got != "/" && bad != "/@evil" {
			t.Errorf("safeNext(%q) = %q", bad, got)
		}
	}
	if safeNext("/targets/1?range=7d") != "/targets/1?range=7d" {
		t.Error("safeNext should keep a local path")
	}
	e := newEnv(t, "")
	c := e.client(t)
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/targets", strings.NewReader("name=x&url=https://x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := c.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site origin should be rejected, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, _ = c.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site fetch metadata should be rejected, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, e.ts.URL+"/logout", nil)
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if resp, _ = c.Do(req); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("same-origin should pass, got %d", resp.StatusCode)
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	e := newEnv(t, "pw")
	c := e.client(t)
	resp, _ := e.post(t, c, "/login", url.Values{"password": {"pw"}})
	var cookie string
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			cookie = ck.Value
		}
	}
	if cookie == "" {
		t.Fatal("no session cookie issued")
	}
	e.post(t, c, "/logout", nil)
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/api/status", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old cookie should be revoked after logout, got %d", r.StatusCode)
	}
	if r, _ := e.post(t, c, "/login", url.Values{"password": {"pw"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatal("successful logins must not count toward the limiter")
	}
}

func TestClientKeyTrustProxy(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = "10.0.0.1:4444"
	// The client claimed 6.6.6.6; our proxy appended the real address.
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if got := clientKey(r, 0); got != "10.0.0.1" {
		t.Errorf("without trust_proxy: %s", got)
	}
	if got := clientKey(r, 1); got != "203.0.113.9" {
		t.Errorf("one trusted proxy should use the rightmost entry, got %s", got)
	}
	// CDN in front of nginx: nginx appends the CDN's address too.
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9, 198.51.100.7")
	if got := clientKey(r, 2); got != "203.0.113.9" {
		t.Errorf("two trusted proxies: %s", got)
	}
	for xff, want := range map[string]string{
		"6.6.6.6, 203.0.113.9:51234": "203.0.113.9",
		"6.6.6.6, [2001:db8::7]:443": "2001:db8::7",
		"6.6.6.6, 2001:db8::8":       "2001:db8::8",
		"6.6.6.6,203.0.113.10":       "203.0.113.10",
	} {
		r.Header.Set("X-Forwarded-For", xff)
		if got := clientKey(r, 1); got != want {
			t.Errorf("X-Forwarded-For %q: got %s, want %s", xff, got, want)
		}
	}
	r.Header.Del("X-Forwarded-For")
	r.Header.Set("X-Real-IP", "203.0.113.5")
	if got := clientKey(r, 1); got != "203.0.113.5" {
		t.Errorf("X-Real-IP fallback: %s", got)
	}
}

func TestLoginLimiterNotBypassedBySpoofedXFF(t *testing.T) {
	e := newEnv(t, "pw")
	e.cfg.TrustProxy = true
	var last int
	for i := 0; i < 8; i++ {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader("password=wrong"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", "10.0.0."+strconv.Itoa(i)+", 9.9.9.9")
		resp, err := e.client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("rotating the spoofable X-Forwarded-For entry must not reset the limit, got %d", last)
	}
}

func TestReviewRegressions(t *testing.T) {
	e := newEnv(t, "pw")
	c := e.client(t)
	// Programmatic requests get 401, not a redirect.
	for _, h := range [][2]string{{"X-Partial", "1"}, {"Accept", "application/json"}, {"Accept", "text/event-stream"}} {
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/", nil)
		req.Header.Set(h[0], h[1])
		if r, _ := c.Do(req); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s=%s should give 401, got %d", h[0], h[1], r.StatusCode)
		}
	}
	// Origin: null is cross-site.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader("password=pw"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "null")
	if r, _ := c.Do(req); r.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin: null should be rejected, got %d", r.StatusCode)
	}
	// Public status page never echoes ?msg=.
	if _, body := e.get(t, c, "/status?msg=All+systems+compromised"); strings.Contains(body, "compromised") {
		t.Fatal("anonymous flash must not render")
	}
	// backTo keeps the query and drops the old flash.
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r.Host = "h"
	r.Header.Set("Referer", "http://h/incidents?ongoing=1&msg=old&sig=x")
	if got := backTo(r, "/"); got != "/incidents?ongoing=1" {
		t.Fatalf("backTo = %q", got)
	}
	r.Header.Set("Referer", "http://other/incidents")
	if got := backTo(r, "/fb"); got != "/fb" {
		t.Fatalf("foreign referer: %q", got)
	}
}

func TestNoteLengthByRunes(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	tg, _ := e.st.CreateTarget(context.Background(), store.Target{Name: "A", URL: "https://a", Interval: time.Hour, Timeout: time.Second}, e.clock)
	id, _ := e.st.OpenIncident(context.Background(), tg.ID, e.clock, "x")
	ok := strings.Repeat("é", 500) // 1000 bytes, 500 chars: allowed
	e.post(t, c, "/incidents/"+strconv.FormatInt(id, 10)+"/note", url.Values{"note": {ok}})
	in, _ := e.st.GetIncident(context.Background(), id)
	if in.Note != ok {
		t.Fatalf("500-rune note should be stored intact (got %d bytes)", len(in.Note))
	}
	resp, _ := e.post(t, c, "/incidents/"+strconv.FormatInt(id, 10)+"/note", url.Values{"note": {ok + "x"}})
	if !strings.Contains(resp.Header.Get("Location"), "500+characters") {
		t.Fatalf("over-long note should be rejected: %s", resp.Header.Get("Location"))
	}
}

func TestMaintenanceWindows(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer up.Close()
	resp, _ := e.post(t, c, "/targets", url.Values{"name": {"API"}, "url": {up.URL}, "interval": {"1h"}, "timeout": {"5s"}, "group": {"api"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create target: %d", resp.StatusCode)
	}
	e.post(t, c, "/targets", url.Values{"name": {"Web"}, "url": {up.URL}, "interval": {"1h"}, "timeout": {"5s"}})

	// Empty page and validation.
	if resp, body := e.get(t, c, "/maintenance"); resp.StatusCode != 200 || !strings.Contains(body, "Nothing scheduled") || !strings.Contains(body, `option value="api"`) {
		t.Fatalf("empty maintenance page: %d", resp.StatusCode)
	}
	if resp, body := e.post(t, c, "/maintenance", url.Values{"title": {""}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "give the window a title") {
		t.Fatalf("missing title: %d", resp.StatusCode)
	}
	if resp, body := e.post(t, c, "/maintenance", url.Values{"title": {"x"}, "duration": {"10s"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "duration") {
		t.Fatalf("short duration: %d", resp.StatusCode)
	}
	if resp, body := e.post(t, c, "/maintenance", url.Values{"title": {"x"}, "starts": {"2026-11-07T09:00"}, "duration": {"1h"}, "tz_offset": {"0"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already over") {
		t.Fatalf("past window: %d", resp.StatusCode)
	}

	// Start one now for the api group only.
	resp, _ = e.post(t, c, "/maintenance", url.Values{"title": {"API deploy"}, "duration": {"2h"}, "group": {"api"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "Maintenance+started") {
		t.Fatalf("start now: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	_, body := e.get(t, c, "/maintenance")
	if !strings.Contains(body, "Active now") || !strings.Contains(body, "API deploy") || !strings.Contains(body, "End now") {
		t.Fatal("active window not shown")
	}
	for _, path := range []string{"/", "/status"} {
		if _, body := e.get(t, c, path); !strings.Contains(body, "aintenance in progress") || !strings.Contains(body, "API deploy") {
			t.Fatalf("%s should show the maintenance banner", path)
		}
	}
	_, body = e.get(t, c, "/api/status")
	if !strings.Contains(body, `"maintenance": "API deploy"`) || strings.Count(body, `"maintenance"`) != 1 {
		t.Fatalf("only the api target should be in maintenance:\n%s", body)
	}
	if _, body := e.get(t, c, "/api/maintenance"); !strings.Contains(body, `"active": true`) || !strings.Contains(body, `"group": "api"`) {
		t.Fatalf("api/maintenance: %s", body)
	}

	// Schedule one for tomorrow 09:00 in a UTC+5:30 browser (offset -330).
	resp, _ = e.post(t, c, "/maintenance", url.Values{"title": {"Disk swap"}, "starts": {"2026-11-08T09:00"}, "duration": {"30m"}, "tz_offset": {"-330"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "scheduled") {
		t.Fatalf("schedule: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	list, _ := e.st.ListMaintenance(context.Background(), e.clock, 0)
	if len(list) != 2 || !list[1].StartsAt.Equal(time.Date(2026, 11, 8, 3, 30, 0, 0, time.UTC)) {
		t.Fatalf("scheduled start should be 03:30 UTC: %+v", list)
	}
	if _, body := e.get(t, c, "/status"); !strings.Contains(body, "Upcoming maintenance") || !strings.Contains(body, "Disk swap") {
		t.Fatal("status page should list the upcoming window")
	}
	if _, body := e.get(t, c, "/maintenance"); !strings.Contains(body, "Upcoming") || !strings.Contains(body, "Disk swap") {
		t.Fatal("maintenance page should list the upcoming window")
	}

	// End the active one, delete the upcoming one.
	active := list[0].ID
	if resp, _ := e.post(t, c, "/maintenance/"+strconv.FormatInt(active, 10)+"/end", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("end: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/api/status"); strings.Contains(body, `"maintenance"`) {
		t.Fatal("nothing should be in maintenance after ending the window")
	}
	if _, body := e.get(t, c, "/"); strings.Contains(body, "Maintenance in progress") {
		t.Fatal("banner should be gone")
	}
	if resp, _ := e.post(t, c, "/maintenance/"+strconv.FormatInt(list[1].ID, 10)+"/delete", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, c, "/maintenance/999/delete", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("delete missing: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/maintenance"); !strings.Contains(body, "Past") || !strings.Contains(body, "API deploy") || strings.Contains(body, "Disk swap") {
		t.Fatal("ended window should be in Past and deleted one gone")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New must reject missing Config, Store and Monitor")
	}
}

func TestLoadByID(t *testing.T) {
	e := newEnv(t, "")
	s := e.srv
	boom := errors.New("boom")
	get := func(_ context.Context, id int64) (string, error) {
		switch id {
		case 1:
			return "one", nil
		case 2:
			return "", boom
		}
		return "", store.ErrNotFound
	}
	for _, tc := range []struct {
		id   string
		code int
		want string
		ok   bool
	}{
		{"1", 200, "one", true}, {"abc", 404, "", false}, {"7", 404, "", false}, {"2", 500, "", false},
	} {
		r := httptest.NewRequest("GET", "/x/"+tc.id, nil)
		r.SetPathValue("id", tc.id)
		w := httptest.NewRecorder()
		got, ok := loadByID(s, w, r, get)
		if ok != tc.ok || got != tc.want {
			t.Errorf("id %q: got (%q, %v), want (%q, %v)", tc.id, got, ok, tc.want, tc.ok)
		}
		if !tc.ok && w.Code != tc.code {
			t.Errorf("id %q: status %d, want %d", tc.id, w.Code, tc.code)
		}
	}
}

func TestPrivateTargetsBlockedWhenNone(t *testing.T) {
	e := newEnv(t, "")
	e.cfg.AllowPrivateTargets = "none"
	c := e.client(t)
	const msg = "blocked: destination is an internal address"
	resp, body := e.post(t, c, "/targets", url.Values{"name": {"x"}, "url": {"http://127.0.0.1:9/"}})
	if resp.StatusCode != 422 || !strings.Contains(body, msg) {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	resp, body = e.post(t, c, "/targets/test", url.Values{"url": {"http://localhost:9/"}})
	if resp.StatusCode != 422 || !strings.Contains(body, msg) {
		t.Fatalf("test: %d %s", resp.StatusCode, body)
	}
	e.cfg.AllowPrivateTargets = "operator"
	resp, body = e.post(t, c, "/targets", url.Values{"name": {"y"}, "url": {"http://127.0.0.1:9/"}})
	if resp.StatusCode != 303 {
		t.Fatalf("operator create: %d %s", resp.StatusCode, body)
	}
	e.cfg.AllowPrivateTargets = "none"
	tgs, _ := e.st.ListTargets(context.Background())
	resp, body = e.post(t, c, "/targets/"+strconv.FormatInt(tgs[0].ID, 10), url.Values{"name": {"y"}, "url": {"http://127.0.0.1:9/"}})
	if resp.StatusCode != 422 || !strings.Contains(body, msg) {
		t.Fatalf("update: %d %s", resp.StatusCode, body)
	}
}
