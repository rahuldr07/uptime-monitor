package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/notify"
	"uptimemon/internal/password"
	"uptimemon/internal/store"
)

const setupAdminPW = "old admin password"

// newSetupEnv builds a server in setup mode with no users. When upgraded is
// true, workspace 1 first gets a target, as a pre-accounts database would.
func newSetupEnv(t *testing.T, upgraded bool, adminPassword string) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, err := config.Parse([]byte(`{"admin_password":` + strconvQuote(adminPassword) + `}`))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{st: st, cfg: cfg, clock: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
		hash: password.New(password.Params{Memory: 64, Time: 1, Threads: 1})}
	now := func() time.Time { return e.clock }
	if upgraded {
		if _, err := st.Workspace(1).CreateTarget(context.Background(),
			store.Target{Name: "old", URL: "http://example.com", Interval: time.Minute, Timeout: time.Second}, now()); err != nil {
			t.Fatal(err)
		}
	}
	e.mon = monitor.New(monitor.Options{Store: st, Notifiers: func(context.Context, int64) (notify.Multi, error) {
		return nil, nil
	}, FailureThreshold: 1, Now: now, PrivateAllowed: func(int64) bool { return true }})
	s, err := New(Options{Config: cfg, Store: st, Monitor: e.mon, Logger: slog.Default(), Now: now,
		Hasher: e.hash, SetupMode: true, Upgraded: upgraded})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = s
	e.ts = httptest.NewServer(s.Handler())
	t.Cleanup(e.ts.Close)
	return e
}

func strconvQuote(s string) string { return `"` + s + `"` }

func (e *env) setupToken(t *testing.T) string {
	t.Helper()
	tok, err := e.st.CreateToken(context.Background(), store.TokenSetup, 0, time.Hour, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func setupForm(extra url.Values) url.Values {
	f := url.Values{"email": {"Boss@Example.com"}, "password": {"a long enough password"},
		"confirm": {"a long enough password"}, "workspace": {"Acme Ops"}}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func (e *env) userCount(t *testing.T) int {
	t.Helper()
	n, err := e.st.CountUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSetupGateEndpoints(t *testing.T) {
	e := newSetupEnv(t, false, "")
	c := e.client(t)
	check := func(path string, code int, loc string) {
		t.Helper()
		resp, _ := e.get(t, c, path)
		if resp.StatusCode != code || resp.Header.Get("Location") != loc {
			t.Errorf("GET %s: %d %q, want %d %q", path, resp.StatusCode, resp.Header.Get("Location"), code, loc)
		}
	}
	check("/", 303, "/setup")
	check("/login", 303, "/setup")
	check("/api/status", 503, "")
	check("/healthz", 200, "")
	check("/s/default", 200, "")
	check("/status", 302, "/s/default")
}

func TestSetupTokenFlow(t *testing.T) {
	e := newSetupEnv(t, false, "")
	tok := e.setupToken(t)
	c := e.client(t)

	resp, body := e.get(t, c, "/setup")
	if resp.StatusCode != 200 || strings.Contains(body, "<form") || !strings.Contains(body, "Open the setup link printed in the server log.") {
		t.Fatalf("setup without token: %d", resp.StatusCode)
	}
	resp, body = e.get(t, c, "/setup?token="+tok)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="token" value="`+tok+`"`) || !strings.Contains(body, `value="Default"`) {
		t.Fatalf("setup form: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if _, err := e.st.PeekToken(context.Background(), store.TokenSetup, tok, e.clock); err != nil {
		t.Fatalf("GET consumed the token: %v", err)
	}

	resp, _ = e.post(t, c, "/setup", setupForm(url.Values{"token": {tok}}))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("setup post: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("post Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if resp, body := e.get(t, c, "/"); resp.StatusCode != 200 || !strings.Contains(body, "boss@example.com") {
		t.Fatalf("session after setup: %d", resp.StatusCode)
	}
	if info, err := e.st.Workspace(1).Info(context.Background()); err != nil || info.Name != "Acme Ops" {
		t.Fatalf("ws1 = %+v, %v", info, err)
	}
	if resp, _ := e.get(t, c, "/setup"); resp.StatusCode != 404 {
		t.Fatalf("/setup after setup: %d", resp.StatusCode)
	}

	// The same token in a fresh environment is unknown there.
	e2 := newSetupEnv(t, false, "")
	if resp, _ := e2.post(t, e2.client(t), "/setup", setupForm(url.Values{"token": {tok}})); resp.StatusCode != 404 {
		t.Fatalf("replayed token: %d", resp.StatusCode)
	}
	// No credential at all.
	if resp, _ := e2.post(t, e2.client(t), "/setup", setupForm(nil)); resp.StatusCode != 404 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if n := e2.userCount(t); n != 0 {
		t.Fatalf("users = %d", n)
	}
}

func TestSetupAfterSetupHook(t *testing.T) {
	e := newSetupEnv(t, false, "")
	called := 0
	e.srv.afterSetup = func(context.Context) error { called++; return context.Canceled }
	resp, _ := e.post(t, e.client(t), "/setup", setupForm(url.Values{"token": {e.setupToken(t)}}))
	if resp.StatusCode != http.StatusSeeOther || called != 1 {
		t.Fatalf("status %d, hook calls %d (a hook error must not fail setup)", resp.StatusCode, called)
	}
}

func TestSetupAdminPasswordOnlyOnUpgraded(t *testing.T) {
	e := newSetupEnv(t, false, setupAdminPW)
	_, body := e.get(t, e.client(t), "/setup")
	if strings.Contains(body, "admin_password") {
		t.Fatal("fresh install offers the admin password")
	}
	if resp, _ := e.post(t, e.client(t), "/setup", setupForm(url.Values{"admin_password": {setupAdminPW}})); resp.StatusCode != 404 {
		t.Fatalf("fresh install with admin password: %d", resp.StatusCode)
	}

	e = newSetupEnv(t, true, setupAdminPW)
	c := e.client(t)
	if _, body := e.get(t, c, "/setup"); !strings.Contains(body, `name="admin_password"`) {
		t.Fatal("upgraded install does not offer the admin password")
	}
	if resp, _ := e.post(t, c, "/setup", setupForm(url.Values{"admin_password": {"wrong"}})); resp.StatusCode != 404 {
		t.Fatalf("wrong admin password: %d", resp.StatusCode)
	}
	resp, _ := e.post(t, c, "/setup", setupForm(url.Values{"admin_password": {setupAdminPW}}))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("right admin password: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, c, "/"); resp.StatusCode != 200 {
		t.Fatalf("session after password setup: %d", resp.StatusCode)
	}
}

func TestSetupLimiter(t *testing.T) {
	e := newSetupEnv(t, false, "")
	for i := 1; i <= 6; i++ {
		resp, _ := e.post(t, e.client(t), "/setup", setupForm(nil))
		want := http.StatusNotFound
		if i == 6 {
			want = http.StatusTooManyRequests
		}
		if resp.StatusCode != want {
			t.Fatalf("POST %d: %d, want %d", i, resp.StatusCode, want)
		}
	}
	e.clock = e.clock.Add(16 * time.Minute)
	if resp, _ := e.post(t, e.client(t), "/setup", setupForm(nil)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after the window: %d", resp.StatusCode)
	}
}

func TestSetupValidation(t *testing.T) {
	e := newSetupEnv(t, false, "")
	tok := e.setupToken(t)
	c := e.client(t)
	long := strings.Repeat("x", 61)
	for name, extra := range map[string]url.Values{
		"mismatch":     {"confirm": {"another long password"}},
		"short":        {"password": {"123456789"}, "confirm": {"123456789"}},
		"bad email":    {"email": {"nope"}},
		"empty name":   {"workspace": {"  "}},
		"long name":    {"workspace": {long}},
		"control name": {"workspace": {"a\x07b"}},
		"format name":  {"workspace": {"a​b"}},
	} {
		e.srv.setupLimiter.Reset("127.0.0.1") // each case is its own attempt budget
		extra.Set("token", tok)
		resp, body := e.post(t, c, "/setup", setupForm(extra))
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `name="token" value="`+tok+`"`) {
			t.Errorf("%s: %d (token must be kept)", name, resp.StatusCode)
		}
	}
	if n := e.userCount(t); n != 0 {
		t.Fatalf("users = %d", n)
	}
	e.srv.setupLimiter.Reset("127.0.0.1")
	if resp, _ := e.post(t, c, "/setup", setupForm(url.Values{"token": {tok}})); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("retry after validation errors: %d", resp.StatusCode)
	}
}

func TestSetupConcurrent(t *testing.T) {
	e := newSetupEnv(t, false, "")
	toks := []string{e.setupToken(t), e.setupToken(t)}
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range toks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := e.client(t).PostForm(e.ts.URL+"/setup", setupForm(url.Values{
				"token": {toks[i]}, "email": {"u" + string(rune('a'+i)) + "@example.com"}}))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}()
	}
	wg.Wait()
	if n := e.userCount(t); n != 1 {
		t.Fatalf("users = %d, codes %v", n, codes)
	}
}
