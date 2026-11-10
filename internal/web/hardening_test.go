package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"uptimemon/internal/store"
)

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t, "pw")
	c := e.client(t)
	for _, path := range []string{"/status", "/login", "/healthz"} {
		resp, _ := e.get(t, c, path)
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self';") || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s missing security headers: %v", path, h)
		}
		if path == "/status" {
			if !strings.Contains(csp, "frame-ancestors *") || h.Get("X-Frame-Options") != "" {
				t.Fatalf("the public status page should be embeddable: %v", h)
			}
		} else if !strings.Contains(csp, "frame-ancestors 'none'") || h.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s must refuse framing: %v", path, h)
		}
		if h.Get("Strict-Transport-Security") != "" {
			t.Fatalf("%s: HSTS is left to the reverse proxy", path)
		}
	}
	// The admin view of /status has the settings form and must not be framed.
	admin := e.client(t)
	e.post(t, admin, "/login", url.Values{"password": {"pw"}})
	if resp, body := e.get(t, admin, "/status"); !strings.Contains(body, "Status page settings") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("admin /status must refuse framing: %v", resp.Header)
	}
	// The strict CSP only works if no template uses inline handlers.
	_, body := e.get(t, c, "/status")
	for _, attr := range []string{"onclick=", "onchange=", "onsubmit=", "<script>"} {
		if strings.Contains(body, attr) {
			t.Fatalf("status page contains inline script %q", attr)
		}
	}
}

func TestForwardedProtoNeedsTrustProxy(t *testing.T) {
	login := func(e *env) *http.Cookie {
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/login", strings.NewReader("password=pw"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", "https")
		resp, err := e.client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookie {
				return c
			}
		}
		t.Fatal("no session cookie")
		return nil
	}
	e := newEnv(t, "pw")
	if login(e).Secure {
		t.Fatal("X-Forwarded-Proto must be ignored unless trust_proxy is set")
	}
	e = newEnv(t, "pw")
	e.cfg.TrustProxy = true
	if !login(e).Secure {
		t.Fatal("behind a trusted proxy, https should give a Secure cookie")
	}
}

func TestAlertSecretsNotEchoed(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	form := url.Values{"tg_token": {"SECRET-TG"}, "tg_chat": {"42"}, "tg_enabled": {"1"},
		"slack_url": {"https://hooks.example/SECRET-SLACK"}, "slack_enabled": {"1"},
		"smtp_host": {"h"}, "smtp_from": {"a@b"}, "smtp_to": {"x@y"}, "smtp_pass": {"SECRET-SMTP"}, "email_enabled": {"1"}}
	if resp, _ := e.post(t, c, "/alerts", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", resp.StatusCode)
	}
	_, body := e.get(t, c, "/alerts")
	for _, secret := range []string{"SECRET-TG", "SECRET-SLACK", "SECRET-SMTP"} {
		if strings.Contains(body, secret) {
			t.Fatalf("alerts page leaks %s", secret)
		}
	}
	if strings.Count(body, "leave blank to keep") != 3 || !strings.Contains(body, `value="42"`) {
		t.Fatal("saved secrets should be indicated, non-secret fields shown")
	}

	// Saving again with the secret fields blank keeps them.
	form.Set("tg_token", "")
	form.Set("slack_url", "")
	form.Set("smtp_pass", "")
	form.Set("tg_chat", "43")
	e.post(t, c, "/alerts", form)
	n := (&Server{store: e.st, cfg: e.cfg}).notifyConfig(context.Background())
	if n.Telegram.BotToken != "SECRET-TG" || n.Telegram.ChatID != "43" || n.Slack.WebhookURL != "https://hooks.example/SECRET-SLACK" || n.Email.Password != "SECRET-SMTP" {
		t.Fatalf("blank secret fields should keep saved values: %+v %+v %+v", n.Telegram, n.Slack, n.Email)
	}

	// The remove box drops a channel even though its secret field is blank.
	form.Set("slack_clear", "1")
	e.post(t, c, "/alerts", form)
	if n := (&Server{store: e.st, cfg: e.cfg}).notifyConfig(context.Background()); n.Slack != nil || n.Telegram == nil {
		t.Fatalf("slack should be removed, telegram kept: %+v", n)
	}
}

func TestStatusPageCachedForVisitors(t *testing.T) {
	e := newEnv(t, "pw")
	anon := e.client(t)
	ctx := context.Background()
	e.st.SetSetting(ctx, "status_title", "First title")
	if _, body := e.get(t, anon, "/status"); !strings.Contains(body, "First title") {
		t.Fatal("status page title")
	}
	e.st.SetSetting(ctx, "status_title", "Second title")
	if _, body := e.get(t, anon, "/status"); !strings.Contains(body, "First title") {
		t.Fatal("anonymous visitors should get the cached page within the TTL")
	}
	// Admins always see live data, and saving settings clears the cache.
	admin := e.client(t)
	e.post(t, admin, "/login", url.Values{"password": {"pw"}})
	if _, body := e.get(t, admin, "/status"); !strings.Contains(body, "Second title") {
		t.Fatal("admins should bypass the cache")
	}
	e.post(t, admin, "/status/settings", url.Values{"title": {"Third title"}})
	if _, body := e.get(t, anon, "/status"); !strings.Contains(body, "Third title") {
		t.Fatal("saving settings should invalidate the cache")
	}
}

func TestStatusCacheNotBypassedByPartialHeader(t *testing.T) {
	e := newEnv(t, "pw")
	ctx := context.Background()
	e.st.SetSetting(ctx, "status_title", "Old title")
	get := func() string {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/status", nil)
		req.Header.Set("X-Partial", "1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	get()
	e.st.SetSetting(ctx, "status_title", "New title")
	if body := get(); !strings.Contains(body, "Old title") {
		t.Fatal("anonymous X-Partial requests must be served from the cache")
	}
}

func TestPageCacheSingleFlightAndStale(t *testing.T) {
	var c pageCache
	ctx := context.Background()
	var builds atomic.Int32
	release := make(chan struct{})
	slow := func() ([]byte, error) {
		builds.Add(1)
		<-release
		return []byte("v1"), nil
	}
	// Ten concurrent first visitors trigger exactly one build.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := c.get(ctx, "k", slow); err != nil || string(b) != "v1" {
				t.Errorf("get: %q %v", b, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := builds.Load(); n != 1 {
		t.Fatalf("want one build for concurrent misses, got %d", n)
	}

	// After a reset, one caller rebuilds while others get the stale copy at once.
	c.reset()
	block := make(chan struct{})
	started := make(chan struct{})
	go c.get(ctx, "k", func() ([]byte, error) { close(started); <-block; return []byte("v2"), nil })
	<-started
	done := make(chan []byte)
	go func() { b, _ := c.get(ctx, "k", slow); done <- b }()
	select {
	case b := <-done:
		if string(b) != "v1" {
			t.Fatalf("want stale v1 while rebuilding, got %q", b)
		}
	case <-time.After(time.Second):
		t.Fatal("a visitor waited on a rebuild instead of getting the stale copy")
	}
	close(block)
	time.Sleep(20 * time.Millisecond)
	if b, _ := c.get(ctx, "k", slow); string(b) != "v2" {
		t.Fatalf("want rebuilt v2, got %q", b)
	}

	// A waiter with no stale copy gives up when its request is cancelled.
	hold := make(chan struct{})
	go c.get(ctx, "other", func() ([]byte, error) { <-hold; return []byte("x"), nil })
	time.Sleep(20 * time.Millisecond)
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.get(cctx, "other", slow); err == nil {
		t.Fatal("waiter should return when its context ends")
	}
	close(hold)
}

func TestPageCacheFailuresAreShared(t *testing.T) {
	var c pageCache
	ctx := context.Background()
	var builds atomic.Int32
	release := make(chan struct{})
	failing := func() ([]byte, error) {
		builds.Add(1)
		<-release
		return nil, fmt.Errorf("database busy")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := c.get(ctx, "k", failing); errs <- err }()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("waiters should get the failed build's error")
		}
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("waiters must share one failed build, not retry it in turn: %d builds", n)
	}
}

func TestPageCachePanicKeepsStaleCopy(t *testing.T) {
	var c pageCache
	ctx := context.Background()
	c.get(ctx, "k", func() ([]byte, error) { return []byte("v1"), nil })
	c.reset()
	func() {
		defer func() { recover() }()
		c.get(ctx, "k", func() ([]byte, error) { panic("boom") })
	}()
	c.mu.Lock()
	p := c.entries["k"]
	body, building := string(p.body), p.building
	c.mu.Unlock()
	if body != "v1" || building != nil {
		t.Fatalf("after a panicking build the stale copy must survive and no build may stay pending: %q %v", body, building)
	}
	if b, err := c.get(ctx, "k", func() ([]byte, error) { return []byte("v2"), nil }); err != nil || string(b) != "v2" {
		t.Fatalf("the next visitor should rebuild: %q %v", b, err)
	}
}

// stalledWriter is a response writer whose Write blocks until released,
// like a client that stops reading once the socket buffers are full.
type stalledWriter struct {
	header  http.Header
	writing chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *stalledWriter) Header() http.Header { return w.header }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.writing) })
	<-w.release
	return len(b), nil
}

func TestSlowStatusReaderDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t, "pw")
	s, err := New(Options{Config: e.cfg, Store: e.st, Monitor: e.mon, Logger: slog.Default(), Now: func() time.Time { return e.clock }})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	slow := &stalledWriter{header: http.Header{}, writing: make(chan struct{}), release: make(chan struct{})}
	defer close(slow.release)
	go h.ServeHTTP(slow, httptest.NewRequest(http.MethodGet, "/status", nil))
	<-slow.writing // the slow visitor is now stuck mid-response

	for _, label := range []string{"cached copy", "rebuild after reset"} {
		done := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
			done <- rec.Code
		}()
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Fatalf("%s: status %d", label, code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: a stalled reader blocked another visitor", label)
		}
		s.statusCache.reset()
	}
}

func TestFlashMustBeSigned(t *testing.T) {
	e := newEnv(t, "") // no password: every visitor is an admin
	c := e.client(t)
	if _, body := e.get(t, c, "/?msg=Session+expired,+log+in+at+evil.example"); strings.Contains(body, "evil.example") {
		t.Fatal("an unsigned ?msg= must not be displayed")
	}
	if _, body := e.get(t, c, "/?msg=Forged&sig="+flashSig("Other")); strings.Contains(body, "Forged") {
		t.Fatal("a signature for another message must not validate")
	}
	if _, body := e.get(t, c, withFlash("/", "Saved fine.")); !strings.Contains(body, "Saved fine.") {
		t.Fatal("a server-signed flash should be displayed")
	}
	// Real flows still show their message.
	resp, _ := e.post(t, c, "/status/settings", url.Values{"title": {"T"}})
	if _, body := e.get(t, c, resp.Header.Get("Location")); !strings.Contains(body, "Status page saved.") {
		t.Fatal("redirect flash lost")
	}
}

func TestHealthzChecksDatabase(t *testing.T) {
	e := newEnv(t, "pw")
	c := e.client(t)
	if resp, body := e.get(t, c, "/healthz"); resp.StatusCode != 200 || strings.TrimSpace(body) != "ok" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}
	e.st.Close()
	if resp, _ := e.get(t, c, "/healthz"); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("healthz with a closed database should be 503, got %d", resp.StatusCode)
	}
}

func TestRequestBodyLimit(t *testing.T) {
	e := newEnv(t, "pw")
	big := url.Values{"password": {strings.Repeat("x", maxBodyBytes+10)}}
	if resp, _ := e.post(t, e.client(t), "/login", big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body should be 413, got %d", resp.StatusCode)
	}
}

func TestThirtyDayUptimeUsesRollups(t *testing.T) {
	e := newEnv(t, "")
	ctx := context.Background()
	tg, _ := e.st.CreateTarget(ctx, store.Target{Name: "A", URL: "https://a", Interval: time.Hour, Timeout: time.Second}, e.clock)
	// 20 days of checks, half failing, then raw checks pruned to the 7-day retention.
	for d := 0; d < 20; d++ {
		at := e.clock.AddDate(0, 0, -d)
		e.st.InsertCheck(ctx, store.Check{TargetID: tg.ID, CheckedAt: at, OK: d%2 == 0, LatencyMS: 10})
	}
	e.st.Prune(ctx, e.clock.Add(-e.cfg.Retention.Duration))
	s := &Server{store: e.st, cfg: e.cfg}
	up30, err := s.uptime(ctx, tg.ID, e.clock, 30)
	if err != nil || up30.Total != 20 || up30.OKCount != 10 {
		t.Fatalf("30d uptime should cover all 20 days from rollups: %v %+v", err, up30)
	}
	up7, _ := s.uptime(ctx, tg.ID, e.clock, 7)
	// Days 0..7 ago inclusive: the check exactly 7 days old is inside the window.
	if up7.Total != 8 {
		t.Fatalf("7d uptime from raw checks: %+v", up7)
	}
}

func TestPasswordEqual(t *testing.T) {
	if !passwordEqual("correct horse", "correct horse") || passwordEqual("correct horse", "correct hors") ||
		passwordEqual("", "x") || passwordEqual("x", "") {
		t.Fatal("passwordEqual")
	}
}

func TestAssetsAreVersioned(t *testing.T) {
	e := newEnv(t, "")
	c := e.client(t)
	for _, page := range []string{"/", "/status"} {
		_, body := e.get(t, c, page)
		for _, name := range []string{"app.js", "app.css"} {
			want := "/static/" + name + "?v=" + assetVersions[name]
			if assetVersions[name] == "" || !strings.Contains(body, want) {
				t.Fatalf("%s should link %s", page, want)
			}
		}
	}
	resp, _ := e.get(t, c, assetURL("app.js"))
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("versioned asset: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	resp, _ = e.get(t, c, "/static/app.js?v=stale")
	if strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatal("a wrong version must not be cached long-term")
	}
}
