package web

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
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
	// Both credentials are valid, so only the in-transaction user check
	// decides the winner.
	e := newSetupEnv(t, true, setupAdminPW)
	forms := []url.Values{
		setupForm(url.Values{"token": {e.setupToken(t)}, "email": {"a@example.com"}}),
		setupForm(url.Values{"admin_password": {setupAdminPW}, "email": {"b@example.com"}}),
	}
	codes := make([]int, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range forms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := e.client(t).PostForm(e.ts.URL+"/setup", forms[i])
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}()
	}
	close(start)
	wg.Wait()
	if !(codes[0] == 303 && codes[1] == 404 || codes[0] == 404 && codes[1] == 303) {
		t.Fatalf("codes = %v, want one 303 and one 404", codes)
	}
	if n := e.userCount(t); n != 1 {
		t.Fatalf("users = %d", n)
	}
}

func TestSetupLimiterCountsSuccessAndSharesPaths(t *testing.T) {
	e := newSetupEnv(t, true, setupAdminPW)
	// Attempts 1-4 mix the token and password paths and fail; attempt 5
	// succeeds. All five share one budget.
	attempts := []url.Values{
		setupForm(url.Values{"token": {"bogus"}}),
		setupForm(url.Values{"admin_password": {"wrong"}}),
		setupForm(url.Values{"token": {"bogus2"}}),
		setupForm(url.Values{"admin_password": {"wrong2"}}),
		setupForm(url.Values{"admin_password": {setupAdminPW}}),
	}
	for i, f := range attempts {
		resp, _ := e.post(t, e.client(t), "/setup", f)
		want := http.StatusNotFound
		if i == 4 {
			want = http.StatusSeeOther
		}
		if resp.StatusCode != want {
			t.Fatalf("attempt %d: %d, want %d", i+1, resp.StatusCode, want)
		}
	}
	// Setup mode is off now, so /setup is a 404; the budget is what counts:
	// the successful attempt used the fifth slot.
	if ok, _ := e.srv.setupLimiter.Take("127.0.0.1"); ok {
		t.Fatal("the successful POST did not count toward the limit")
	}
}

func TestSetupValidationWithoutCredentialHasHint(t *testing.T) {
	e := newSetupEnv(t, false, "")
	resp, body := e.post(t, e.client(t), "/setup", setupForm(url.Values{"confirm": {"different password"}}))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Open the setup link printed in the server log.") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

// loginFrom posts a sign-in as if from addr (needs trust_proxy), optionally
// carrying extra cookies.
func (e *env) loginFrom(t *testing.T, addr, email, pw string, cookies ...*http.Cookie) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login", strings.NewReader(url.Values{"email": {email}, "password": {pw}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", addr)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestLoginPerAddressLimit(t *testing.T) {
	e := newEnv(t)
	e.cfg.TrustProxy = true
	for i := 0; i < 5; i++ {
		if resp, _ := e.loginFrom(t, "203.0.113.1", opEmail, "wrong"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, resp.StatusCode)
		}
	}
	resp, body := e.loginFrom(t, "203.0.113.1", opEmail, "wrong")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgTooMany) {
		t.Fatalf("6th from one address: %d", resp.StatusCode)
	}
	if resp, _ := e.loginFrom(t, "203.0.113.2", opEmail, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("another address: %d", resp.StatusCode)
	}
}

func TestLoginPerPairLimit(t *testing.T) {
	e := newEnv(t)
	e.cfg.TrustProxy = true
	const pairEmail = "a@x.example"
	// Two failures per minute from each address keep the per-address limit
	// out of the way; advance the clock between batches.
	for i := 0; i < 10; i++ {
		if i%4 == 0 && i > 0 {
			e.clock = e.clock.Add(2 * time.Minute)
		}
		if resp, _ := e.loginFrom(t, "203.0.113.50", pairEmail, "wrong"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, resp.StatusCode)
		}
	}
	e.clock = e.clock.Add(2 * time.Minute)
	resp, body := e.loginFrom(t, "203.0.113.50", pairEmail, "wrong")
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgTooMany) {
		t.Fatalf("11th for the pair: %d", resp.StatusCode)
	}
	// Same email from another address is not blocked by the pair limit.
	if resp, _ := e.loginFrom(t, "203.0.113.51", pairEmail, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other address, same email: %d", resp.StatusCode)
	}
	// Same address, other email: not blocked either.
	if resp, _ := e.loginFrom(t, "203.0.113.50", "b@x.example", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("same address, other email: %d", resp.StatusCode)
	}
}

func TestLoginPerEmailSlowdown(t *testing.T) {
	e := newEnv(t)
	e.cfg.TrustProxy = true
	addr := func(i int) string { return "198.51.100." + strconv.Itoa(i) }
	for i := 1; i <= 21; i++ {
		if resp, _ := e.loginFrom(t, addr(i), opEmail, "wrong"); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, resp.StatusCode)
		}
	}
	// Slowed: the next attempt inside the 5s gate is refused, even with the
	// right password and a fresh address.
	resp, body := e.loginFrom(t, addr(100), opEmail, testSecret)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgTooMany) {
		t.Fatalf("slowed attempt: %d", resp.StatusCode)
	}
	// After the gate period one attempt goes through: never locked out.
	e.clock = e.clock.Add(5 * time.Second)
	resp, _ = e.loginFrom(t, addr(101), opEmail, testSecret)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after 5s the correct password must succeed, got %d", resp.StatusCode)
	}
	dev := cookieNamed(resp, deviceCookie)
	if dev == nil || !dev.HttpOnly || dev.SameSite != http.SameSiteLaxMode || dev.MaxAge != 90*24*3600 || dev.Secure {
		t.Fatalf("device cookie = %+v", dev)
	}
	// The gate is spent again; without the device cookie we are held back.
	if resp, _ := e.loginFrom(t, addr(102), opEmail, testSecret); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second attempt in the gate window: %d", resp.StatusCode)
	}
	// A known device skips the slowdown at once.
	if resp, _ := e.loginFrom(t, addr(103), opEmail, testSecret, dev); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("known device: %d", resp.StatusCode)
	}
	// A device cookie for another user, or a forged one, does not help.
	for _, v := range []string{"2." + strings.SplitN(dev.Value, ".", 2)[1], "1.deadbeef", "garbage"} {
		bad := &http.Cookie{Name: deviceCookie, Value: v}
		if resp, _ := e.loginFrom(t, addr(104), opEmail, testSecret, bad); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("cookie %q: %d", v, resp.StatusCode)
		}
	}
}

func TestLoginNoEnumeration(t *testing.T) {
	if testing.Short() {
		t.Skip("uses the real password hasher")
	}
	e := newEnvWith(t, password.Default())
	e.cfg.TrustProxy = true
	const n = 30
	var known, unknown []time.Duration
	var knownBody, unknownBody string
	for i := 0; i < n+1; i++ {
		// A fresh address per request; the clock moves past every window.
		e.clock = e.clock.Add(time.Hour)
		for _, c := range []struct {
			email string
			out   *[]time.Duration
			body  *string
		}{{opEmail, &known, &knownBody}, {"nobody" + strconv.Itoa(i) + "@example.com", &unknown, &unknownBody}} {
			addr := "192.0.2." + strconv.Itoa(2*i+1)
			if c.out == &unknown {
				addr = "192.0.2." + strconv.Itoa(2*i+2)
			}
			start := time.Now()
			resp, body := e.loginFrom(t, addr, c.email, "wrong password!")
			d := time.Since(start)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s: %d", c.email, resp.StatusCode)
			}
			if i == 0 {
				*c.body = strings.ReplaceAll(body, c.email, "")
				continue // warm-up
			}
			*c.out = append(*c.out, d)
		}
		if i == 0 && knownBody != strings.ReplaceAll(unknownBody, "nobody0@example.com", "") {
			t.Fatalf("bodies differ:\n%s\n---\n%s", knownBody, unknownBody)
		}
	}
	med := func(ds []time.Duration) time.Duration {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	mk, mu := med(known), med(unknown)
	t.Logf("median known=%v unknown=%v", mk, mu)
	diff := mk - mu
	if diff < 0 {
		diff = -diff
	}
	if diff >= 50*time.Millisecond {
		t.Fatalf("medians differ by %v (known %v, unknown %v)", diff, mk, mu)
	}
}

func TestDisabledOnlyAfterCorrectPassword(t *testing.T) {
	e := newEnv(t)
	e.account(t, "dis@example.com")
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE users SET disabled_at=? WHERE email=?`, e.clock.UnixMilli(), "dis@example.com"); err != nil {
		t.Fatal(err)
	}
	c := e.client(t)
	if resp, body := e.post(t, c, "/login", url.Values{"email": {"dis@example.com"}, "password": {"wrong"}}); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, msgBadLogin) {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	if resp, body := e.post(t, c, "/login", url.Values{"email": {"dis@example.com"}, "password": {testSecret}}); resp.StatusCode != http.StatusForbidden || !strings.Contains(body, msgDisabled) {
		t.Fatalf("correct password: %d", resp.StatusCode)
	}
}

func TestHashBusyGives503(t *testing.T) {
	// A zero Hasher has no run slots at all, so every acquire gives ErrBusy.
	e := newEnvWith(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}))
	e.srv.hasher = &password.Hasher{}
	resp, body := e.post(t, e.client(t), "/login", url.Values{"email": {opEmail}, "password": {testSecret}})
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, msgBusy) {
		t.Fatalf("busy hasher: %d", resp.StatusCode)
	}
}
