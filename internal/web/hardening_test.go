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
	e := newEnv(t)
	c := e.client(t)
	for _, path := range []string{"/s/default", "/login", "/healthz"} {
		resp, _ := e.get(t, c, path)
		h := resp.Header
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self';") || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s missing security headers: %v", path, h)
		}
		if path == "/s/default" {
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
	admin := e.op
	if resp, body := e.get(t, admin, "/s/default"); !strings.Contains(body, "Status page settings") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("admin /status must refuse framing: %v", resp.Header)
	}
	// The strict CSP only works if no template uses inline handlers.
	_, body := e.get(t, c, "/s/default")
	for _, attr := range []string{"onclick=", "onchange=", "onsubmit=", "<script>"} {
		if strings.Contains(body, attr) {
			t.Fatalf("status page contains inline script %q", attr)
		}
	}
}

func TestForwardedProtoNeedsTrustProxy(t *testing.T) {
	login := func(e *env) *http.Cookie {
		req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader("email="+opEmail+"&password="+url.QueryEscape(testSecret)))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-Proto", "https")
		resp, err := e.client(t).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookie || c.Name == hostSessionCookie {
				return c
			}
		}
		t.Fatal("no session cookie")
		return nil
	}
	e := newEnv(t)
	if login(e).Secure {
		t.Fatal("X-Forwarded-Proto must be ignored unless trust_proxy is set")
	}
	e = newEnv(t)
	e.cfg.TrustProxy = true
	if !login(e).Secure {
		t.Fatal("behind a trusted proxy, https should give a Secure cookie")
	}
}

func TestAlertSecretsNotEchoed(t *testing.T) {
	e := newEnv(t)
	c := e.op
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
	n := (&Server{sys: e.st, cfg: e.cfg}).notifyConfig(context.Background(), e.st.Workspace(1))
	if n.Telegram.BotToken != "SECRET-TG" || n.Telegram.ChatID != "43" || n.Slack.WebhookURL != "https://hooks.example/SECRET-SLACK" || n.Email.Password != "SECRET-SMTP" {
		t.Fatalf("blank secret fields should keep saved values: %+v %+v %+v", n.Telegram, n.Slack, n.Email)
	}

	// The remove box drops a channel even though its secret field is blank.
	form.Set("slack_clear", "1")
	e.post(t, c, "/alerts", form)
	if n := (&Server{sys: e.st, cfg: e.cfg}).notifyConfig(context.Background(), e.st.Workspace(1)); n.Slack != nil || n.Telegram == nil {
		t.Fatalf("slack should be removed, telegram kept: %+v", n)
	}
}

func TestStatusPageCachedForVisitors(t *testing.T) {
	e := newEnv(t)
	anon := e.client(t)
	ctx := context.Background()
	e.st.Workspace(1).SetSetting(ctx, "status_title", "First title")
	if _, body := e.get(t, anon, "/s/default"); !strings.Contains(body, "First title") {
		t.Fatal("status page title")
	}
	e.st.Workspace(1).SetSetting(ctx, "status_title", "Second title")
	if _, body := e.get(t, anon, "/s/default"); !strings.Contains(body, "First title") {
		t.Fatal("anonymous visitors should get the cached page within the TTL")
	}
	// Admins always see live data, and saving settings clears the cache.
	admin := e.op
	if _, body := e.get(t, admin, "/s/default"); !strings.Contains(body, "Second title") {
		t.Fatal("admins should bypass the cache")
	}
	e.post(t, admin, "/status/settings", url.Values{"title": {"Third title"}})
	if _, body := e.get(t, anon, "/s/default"); !strings.Contains(body, "Third title") {
		t.Fatal("saving settings should invalidate the cache")
	}
}

func TestStatusCacheNotBypassedByPartialHeader(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.st.Workspace(1).SetSetting(ctx, "status_title", "Old title")
	get := func() string {
		req, _ := http.NewRequest(http.MethodGet, e.ts.URL+"/s/default", nil)
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
	e.st.Workspace(1).SetSetting(ctx, "status_title", "New title")
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
	c.reset("k")
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
	c.reset("k")
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
	e := newEnv(t)
	s, err := New(Options{Config: e.cfg, Store: e.st, Monitor: e.mon, Logger: slog.Default(), Now: func() time.Time { return e.clock }})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	slow := &stalledWriter{header: http.Header{}, writing: make(chan struct{}), release: make(chan struct{})}
	defer close(slow.release)
	go h.ServeHTTP(slow, httptest.NewRequest(http.MethodGet, "/s/default", nil))
	<-slow.writing // the slow visitor is now stuck mid-response

	for _, label := range []string{"cached copy", "rebuild after reset"} {
		done := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s/default", nil))
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
		s.statusCache.reset("default")
	}
}

func TestFlashMustBeSigned(t *testing.T) {
	e := newEnv(t) // signed in as the operator
	c := e.op
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
	e := newEnv(t)
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
	e := newEnv(t)
	big := url.Values{"password": {strings.Repeat("x", maxBodyBytes+10)}}
	if resp, _ := e.post(t, e.client(t), "/login", big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body should be 413, got %d", resp.StatusCode)
	}
}

func TestThirtyDayUptimeUsesRollups(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	tg, _ := e.st.Workspace(1).CreateTarget(ctx, store.Target{Name: "A", URL: "https://a", Interval: time.Hour, Timeout: time.Second}, e.clock)
	// 20 days of checks, half failing, then raw checks pruned to the 7-day retention.
	for d := 0; d < 20; d++ {
		at := e.clock.AddDate(0, 0, -d)
		e.st.InsertCheck(ctx, store.Check{TargetID: tg.ID, CheckedAt: at, OK: d%2 == 0, LatencyMS: 10})
	}
	e.st.Prune(ctx, e.clock.Add(-e.cfg.Retention.Duration))
	s := &Server{sys: e.st, cfg: e.cfg}
	up30, err := s.uptime(ctx, e.st.Workspace(1), tg.ID, e.clock, 30)
	if err != nil || up30.Total != 20 || up30.OKCount != 10 {
		t.Fatalf("30d uptime should cover all 20 days from rollups: %v %+v", err, up30)
	}
	up7, _ := s.uptime(ctx, e.st.Workspace(1), tg.ID, e.clock, 7)
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
	e := newEnv(t)
	c := e.op
	for _, page := range []string{"/", "/s/default"} {
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

func TestStatusSlugRouting(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	resp, _ := e.get(t, c, "/status")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/s/default" {
		t.Fatalf("/status: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := e.get(t, c, "/s/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown slug: %d", resp.StatusCode)
	}
	e.srv.statusCache.mu.Lock()
	for k := range e.srv.statusCache.entries {
		if strings.HasPrefix(k, "nope") {
			t.Errorf("unknown slug reached the cache: %q", k)
		}
	}
	e.srv.statusCache.mu.Unlock()
	resp, _ = e.get(t, c, "/s/default")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors *") {
		t.Fatalf("/s/default: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestStatusCacheBounded(t *testing.T) {
	c := pageCache{maxBytes: 100}
	ctx := context.Background()
	put := func(key string, n int) {
		t.Helper()
		if _, err := c.get(ctx, key, func() ([]byte, error) { return make([]byte, n), nil }); err != nil {
			t.Fatal(err)
		}
	}
	put("a|1", 40)
	put("b|1", 40)
	put("a|1", 40) // a|1 is now the most recently used
	put("c|1", 40) // 120 > 100: evicts b|1, the least recently used
	c.mu.Lock()
	_, hasA := c.entries["a|1"]
	_, hasB := c.entries["b|1"]
	_, hasC := c.entries["c|1"]
	total := c.total
	c.mu.Unlock()
	if total > 100 || !hasA || hasB || !hasC {
		t.Fatalf("total=%d a=%v b=%v c=%v", total, hasA, hasB, hasC)
	}

	// reset("a") stales only a's entries.
	c.reset("a")
	var builds atomic.Int32
	build := func() ([]byte, error) { builds.Add(1); return make([]byte, 40), nil }
	c.get(ctx, "c|1", build)
	if builds.Load() != 0 {
		t.Fatal("reset(a) must not invalidate c")
	}
	c.get(ctx, "a|1", build)
	if builds.Load() != 1 {
		t.Fatal("reset(a) must invalidate a")
	}
}

func TestStatusRebuildCap(t *testing.T) {
	var c pageCache
	ctx := context.Background()
	var running, peak atomic.Int32
	release := make(chan struct{})
	build := func() ([]byte, error) {
		n := running.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		running.Add(-1)
		return []byte("x"), nil
	}
	var wg sync.WaitGroup
	for _, k := range []string{"a|1", "b|1", "c|1"} {
		wg.Add(1)
		go func() { defer wg.Done(); c.get(ctx, k, build) }()
	}
	time.Sleep(100 * time.Millisecond)
	if n := running.Load(); n != statusMaxRebuilds {
		t.Fatalf("running builds = %d, want %d", n, statusMaxRebuilds)
	}
	close(release)
	wg.Wait()
	if peak.Load() > statusMaxRebuilds {
		t.Fatalf("peak concurrent builds = %d", peak.Load())
	}
}

func TestStatusRateLimited(t *testing.T) {
	e := newEnv(t)
	c := e.client(t)
	for i := 1; i <= 120; i++ {
		if resp, _ := e.get(t, c, "/s/default"); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	resp, body := e.get(t, c, "/s/default")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || body != "too many requests" {
		t.Fatalf("121st: %d %q %q", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	admin := e.op
	if resp, _ := e.get(t, admin, "/s/default"); resp.StatusCode != 200 {
		t.Fatalf("admin must not be limited: %d", resp.StatusCode)
	}
}

func TestStatusCacheNeverEvictsBuilding(t *testing.T) {
	c := pageCache{maxBytes: 100}
	ctx := context.Background()
	c.get(ctx, "a|1", func() ([]byte, error) { return make([]byte, 60), nil })
	c.reset("a")
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.get(ctx, "a|1", func() ([]byte, error) { close(started); <-release; return make([]byte, 60), nil })
	}()
	<-started // a|1 is rebuilding and is the least recently used entry
	c.get(ctx, "b|1", func() ([]byte, error) { return make([]byte, 60), nil })
	c.mu.Lock()
	_, hasA := c.entries["a|1"]
	c.mu.Unlock()
	if !hasA {
		t.Fatal("an entry that is rebuilding must not be evicted")
	}
	close(release)
	<-done
}

func TestStatusBuildSurvivesCancelledLeader(t *testing.T) {
	var c pageCache
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	started, release := make(chan struct{}), make(chan struct{})
	var buildErr atomic.Value
	build := func() ([]byte, error) {
		bctx, cancel := buildCtx(leaderCtx)
		defer cancel()
		close(started)
		<-release
		if err := bctx.Err(); err != nil {
			buildErr.Store(err)
			return nil, err
		}
		return []byte("page"), nil
	}
	go c.get(leaderCtx, "k", build)
	<-started
	waiterDone := make(chan error, 1)
	var got atomic.Value
	go func() {
		b, err := c.get(context.Background(), "k", build)
		got.Store(string(b))
		waiterDone <- err
	}()
	// Wait until the waiter is parked on the flight, then cancel the leader.
	for {
		c.mu.Lock()
		f := c.entries["k"].building
		c.mu.Unlock()
		if f != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancelLeader()
	close(release)
	if err := <-waiterDone; err != nil || got.Load() != "page" {
		t.Fatalf("waiter: %v %v", got.Load(), err)
	}
	if v := buildErr.Load(); v != nil {
		t.Fatalf("build ctx was cancelled with the leader: %v", v)
	}
}
