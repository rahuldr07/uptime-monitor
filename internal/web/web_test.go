package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/notify"
	"uptimemon/internal/store"
)

type env struct {
	srv   *httptest.Server
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
		FailureThreshold: 1, Now: now})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := e.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, st, e.mon, slog.Default(), now)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(s.Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) client(t *testing.T) *http.Client {
	jar := &cookieJar{}
	return &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *env) get(t *testing.T, c *http.Client, path string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(e.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func (e *env) post(t *testing.T, c *http.Client, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(e.srv.URL+path, form)
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
	if resp, body := e.get(t, c, "/status"); resp.StatusCode != 200 || !strings.Contains(body, "All systems operational") {
		t.Fatalf("public status page: %d", resp.StatusCode)
	}
	if resp, body := e.post(t, c, "/login", url.Values{"password": {"wrong"}}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "Wrong password") {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = e.post(t, c, "/login", url.Values{"password": {"secret"}, "next": {"//evil.com"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login redirect should be sanitised: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := e.get(t, c, "/"); resp.StatusCode != 200 || !strings.Contains(body, "No targets yet") {
		t.Fatalf("dashboard after login: %d", resp.StatusCode)
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
	if resp, body := e.get(t, c, "/partials/dashboard"); resp.StatusCode != 200 || strings.Contains(body, "<html") {
		t.Fatal("partial should not include layout")
	}
	if _, body := e.get(t, c, loc+"?range=7d"); !strings.Contains(body, `class="active">7d`) {
		t.Fatal("range switch")
	}
	if resp, _ := e.post(t, c, loc, url.Values{"name": {"Site2"}, "url": {up.URL}, "interval": {"2h"}, "timeout": {"5s"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("update: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/targets"); !strings.Contains(body, "Site2") || !strings.Contains(body, "2h0m0s") {
		t.Fatal("targets page should show updated target")
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
	resp, _ = e.post(t, c, "/alerts", url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "smtp_host": {"h"}, "smtp_from": {"a@b"}, "smtp_to": {"x@y, z@w"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	names := []string{}
	for _, n := range e.mon.Notifiers() {
		names = append(names, n.Name)
	}
	if strings.Join(names, ",") != "telegram,email" {
		t.Fatalf("notifiers after save: %v", names)
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
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/targets", strings.NewReader("name=x&url=https://x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := c.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site origin should be rejected, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, _ = c.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site fetch metadata should be rejected, got %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/logout", nil)
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
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/status", nil)
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
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if clientKey(r, false) != "10.0.0.1" || clientKey(r, true) != "203.0.113.9" {
		t.Errorf("clientKey: %s / %s", clientKey(r, false), clientKey(r, true))
	}
}
