package web

import (
	"strconv"

	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uptimemon/internal/store"
)

func addTarget(t *testing.T, e *env, c *http.Client, name, rawURL, interval string) (*http.Response, string) {
	t.Helper()
	return e.post(t, c, "/targets", url.Values{"name": {name}, "url": {rawURL}, "interval": {interval}, "timeout": {"1s"}})
}

func TestMonitorCapAndInterval(t *testing.T) {
	e := newEnv(t)
	two := 2
	e.cfg.Limits.MaxMonitors = &two
	c, _ := e.account(t, "a@example.com")

	if resp, body := addTarget(t, e, c, "fast", "https://a.example/", "10s"); resp.StatusCode != 422 || !strings.Contains(body, "Check interval must be at least 30s.") {
		t.Fatalf("10s interval: %d %s", resp.StatusCode, body)
	}
	for i, h := range []string{"a1", "a2"} {
		if resp, body := addTarget(t, e, c, h, "https://"+h+".example/", "1h"); resp.StatusCode != 303 {
			t.Fatalf("create %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, body := addTarget(t, e, c, "a3", "https://a3.example/", "1h"); resp.StatusCode != 422 || !strings.Contains(body, "This workspace has reached its limit of 2 monitors.") {
		t.Fatalf("3rd create: %d %s", resp.StatusCode, body)
	}
	// The operator is exempt from both.
	for _, h := range []string{"o1", "o2", "o3"} {
		if resp, body := addTarget(t, e, e.op, h, "https://"+h+".example/", "5s"); resp.StatusCode != 303 {
			t.Fatalf("operator create %s: %d %s", h, resp.StatusCode, body)
		}
	}
}

func TestMonitorUpdateInterval(t *testing.T) {
	e := newEnv(t)
	c, ws := e.account(t, "a@example.com")
	if resp, body := addTarget(t, e, c, "x", "https://x.example/", "1h"); resp.StatusCode != 303 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	ts, _ := ws.ListTargets(t.Context())
	path := "/targets/" + itoa(ts[0].ID)
	resp, body := e.post(t, c, path, url.Values{"name": {"x"}, "url": {"https://x.example/"}, "interval": {"5s"}, "timeout": {"1s"}})
	if resp.StatusCode != 422 || !strings.Contains(body, "Check interval must be at least 30s.") {
		t.Fatalf("update 5s: %d %s", resp.StatusCode, body)
	}
	resp, body = e.post(t, c, path, url.Values{"name": {"x"}, "url": {"https://x.example/"}, "interval": {"2m"}, "timeout": {"1s"}})
	if resp.StatusCode != 303 {
		t.Fatalf("update 2m: %d %s", resp.StatusCode, body)
	}
}

func TestCreateRate(t *testing.T) {
	e := newEnv(t)
	zero := 0
	e.cfg.Limits.MaxMonitors = &zero
	c, ws := e.account(t, "a@example.com")
	for i := 0; i < 30; i++ {
		resp, body := addTarget(t, e, c, "m", "https://h"+itoa(int64(i))+".example/", "1h")
		if resp.StatusCode != 303 {
			t.Fatalf("create %d: %d %s", i, resp.StatusCode, body)
		}
		ts, _ := ws.ListTargets(t.Context())
		if err := ws.DeleteTarget(t.Context(), ts[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	resp, body := addTarget(t, e, c, "m", "https://late.example/", "1h")
	if resp.StatusCode != 429 || !strings.Contains(body, "You&#39;re creating monitors too quickly.") {
		t.Fatalf("31st create: %d %s", resp.StatusCode, body)
	}
	if resp, body := addTarget(t, e, e.op, "m", "https://late.example/", "1h"); resp.StatusCode != 303 {
		t.Fatalf("operator: %d %s", resp.StatusCode, body)
	}
	e.advance(61 * time.Minute)
	if resp, body := addTarget(t, e, c, "m", "https://late.example/", "1h"); resp.StatusCode != 303 {
		t.Fatalf("after window: %d %s", resp.StatusCode, body)
	}
}

func TestHostRateLimitAcrossWorkspaces(t *testing.T) {
	e := newEnv(t)
	e.cfg.Limits.MinInterval.Duration = time.Second
	b, _ := e.account(t, "b@example.com")
	c, cws := e.account(t, "c@example.com")
	for _, cl := range []*http.Client{b, c} {
		if resp, body := addTarget(t, e, cl, "s", "https://Same.example/", "2s"); resp.StatusCode != 303 {
			t.Fatalf("2s add: %d %s", resp.StatusCode, body)
		}
	}
	resp, body := addTarget(t, e, c, "s2", "https://same.example/x", "10s")
	if resp.StatusCode != 422 || !strings.Contains(body, "Too many checks to this host from this instance. Use a longer interval.") {
		t.Fatalf("over rate: %d %s", resp.StatusCode, body)
	}
	// A trailing dot or different case names the same host.
	if resp, _ := addTarget(t, e, c, "dot", "https://same.example./", "10s"); resp.StatusCode != 422 {
		t.Fatalf("trailing dot bypassed the host rate: %d", resp.StatusCode)
	}
	if resp, body := addTarget(t, e, c, "other", "https://other.example/", "10s"); resp.StatusCode != 303 {
		t.Fatalf("other host: %d %s", resp.StatusCode, body)
	}
	// The operator's targets neither count nor are limited.
	if resp, body := addTarget(t, e, e.op, "o", "https://same.example/", "1s"); resp.StatusCode != 303 {
		t.Fatalf("operator: %d %s", resp.StatusCode, body)
	}
	if resp, body := addTarget(t, e, e.op, "o2", "https://same.example/", "1s"); resp.StatusCode != 303 {
		t.Fatalf("operator 2: %d %s", resp.StatusCode, body)
	}
	// Updating counts the others but not the target's own current row.
	ts, _ := cws.ListTargets(t.Context())
	var own int64
	for _, x := range ts {
		if x.Name == "s" {
			own = x.ID
		}
	}
	path := "/targets/" + itoa(own)
	if resp, body := e.post(t, c, path, url.Values{"name": {"s"}, "url": {"https://same.example/"}, "interval": {"2s"}, "timeout": {"1s"}}); resp.StatusCode != 303 {
		t.Fatalf("update same: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.post(t, c, path, url.Values{"name": {"s"}, "url": {"https://same.example/"}, "interval": {"1s"}, "timeout": {"500ms"}}); resp.StatusCode != 422 || !strings.Contains(body, "Too many checks to this host from this instance.") {
		t.Fatalf("update faster: %d %s", resp.StatusCode, body)
	}
	// Moving a monitor to a busy host is checked against that host.
	var other int64
	ts, _ = cws.ListTargets(t.Context())
	for _, x := range ts {
		if x.Name == "other" {
			other = x.ID
		}
	}
	if resp, body := e.post(t, c, "/targets/"+itoa(other), url.Values{"name": {"other"}, "url": {"https://SAME.example/"}, "interval": {"2s"}, "timeout": {"500ms"}}); resp.StatusCode != 422 || !strings.Contains(body, "Too many checks to this host from this instance.") {
		t.Fatalf("hostname change: %d %s", resp.StatusCode, body)
	}
}

func TestHostRateCountsOwnWorkspace(t *testing.T) {
	e := newEnv(t)
	e.cfg.Limits.MinInterval.Duration = time.Second
	c, _ := e.account(t, "a@example.com")
	for i := 0; i < 2; i++ {
		if resp, body := addTarget(t, e, c, "s"+itoa(int64(i)), "https://same.example/"+itoa(int64(i)), "2s"); resp.StatusCode != 303 {
			t.Fatalf("add %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, _ := addTarget(t, e, c, "s", "https://same.example/z", "10s"); resp.StatusCode != 422 {
		t.Fatalf("own-workspace sum not counted: %d", resp.StatusCode)
	}
}

func TestCheckNowLimitsAndCoalescing(t *testing.T) {
	e := newEnv(t)
	zero := 0
	e.cfg.Limits.MaxMonitors = &zero
	e.cfg.AllowPrivateTargets = "all"
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	c, ws := e.account(t, "a@example.com")
	if resp, body := addTarget(t, e, c, "one", up.URL, "1h"); resp.StatusCode != 303 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	waitHits := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for hits.Load() != n {
			if time.Now().After(deadline) {
				t.Fatalf("hits = %d, want %d", hits.Load(), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitHits(1)
	ts, _ := ws.ListTargets(t.Context())
	path := "/targets/" + itoa(ts[0].ID) + "/check"
	if resp, _ := e.post(t, c, path, nil); resp.StatusCode != 303 {
		t.Fatalf("first check: %d", resp.StatusCode)
	}
	waitHits(2)
	resp, _ := e.post(t, c, path, nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second check: %d", resp.StatusCode)
	}
	time.Sleep(100 * time.Millisecond)
	waitHits(2)

	// Workspace budget: 30 per minute across targets (one was spent above).
	for i := 0; i < 29; i++ {
		tg, err := ws.CreateTarget(t.Context(), storeTarget("t"+itoa(int64(i)), up.URL), e.now())
		if err != nil {
			t.Fatal(err)
		}
		if err := e.mon.Reload(t.Context(), tg.ID); err != nil {
			t.Fatal(err)
		}
		p := "/targets/" + itoa(tg.ID) + "/check"
		if resp, _ := e.post(t, c, p, nil); resp.StatusCode != 303 {
			t.Fatalf("check %d: %d", i, resp.StatusCode)
		}
	}
	tg, _ := ws.CreateTarget(t.Context(), storeTarget("last", up.URL), e.now())
	if resp, _ := e.post(t, c, "/targets/"+itoa(tg.ID)+"/check", nil); resp.StatusCode != 429 {
		t.Fatalf("31st check: %d", resp.StatusCode)
	}
	// The operator is exempt.
	otg, _ := e.opWS.CreateTarget(t.Context(), storeTarget("op", up.URL), e.now())
	if err := e.mon.Reload(t.Context(), otg.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if resp, _ := e.post(t, e.op, "/targets/"+itoa(otg.ID)+"/check", nil); resp.StatusCode != 303 {
			t.Fatalf("operator check %d: %d", i, resp.StatusCode)
		}
	}
}

func TestTestURLAndAlertTestLimits(t *testing.T) {
	e := newEnv(t)
	e.cfg.AllowPrivateTargets = "all"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	c, _ := e.account(t, "a@example.com")
	for i := 0; i < 10; i++ {
		resp, body := e.post(t, c, "/targets/test", url.Values{"url": {up.URL}})
		if resp.StatusCode != 200 && resp.StatusCode != 422 {
			t.Fatalf("test url %d: %d %s", i, resp.StatusCode, body)
		}
		if resp.StatusCode == 429 {
			t.Fatalf("limited early at %d", i)
		}
	}
	resp, body := e.post(t, c, "/targets/test", url.Values{"url": {up.URL}})
	if resp.StatusCode != 429 || !strings.Contains(resp.Header.Get("Content-Type"), "json") || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("11th test url: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.post(t, e.op, "/targets/test", url.Values{"url": {up.URL}}); resp.StatusCode == 429 {
		t.Fatal("operator limited")
	}

	for i := 0; i < 5; i++ {
		if resp, _ := e.post(t, c, "/alerts/test", url.Values{"channel": {"log"}}); resp.StatusCode == 429 {
			t.Fatalf("alert test limited early at %d", i)
		}
	}
	if resp, body := e.post(t, c, "/alerts/test", url.Values{"channel": {"log"}}); resp.StatusCode != 429 {
		t.Fatalf("6th alert test: %d %s", resp.StatusCode, body)
	}
	for i := 0; i < 7; i++ {
		if resp, _ := e.post(t, e.op, "/alerts/test", url.Values{"channel": {"log"}}); resp.StatusCode == 429 {
			t.Fatal("operator limited")
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func storeTarget(name, u string) store.Target {
	return store.Target{Name: name, URL: u, Interval: time.Hour, Timeout: 5 * time.Second}
}

func TestFailedCreateSpendsSlot(t *testing.T) {
	e := newEnv(t)
	c, _ := e.account(t, "a@example.com")
	for i := 0; i < 30; i++ {
		if resp, _ := addTarget(t, e, c, "bad", "ftp://x", "1h"); resp.StatusCode != 422 {
			t.Fatalf("invalid create %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := addTarget(t, e, c, "good", "https://good.example/", "1h"); resp.StatusCode != 429 {
		t.Fatalf("31st create after 30 failures: %d", resp.StatusCode)
	}
}

func TestCheckNowCoalescedFlash(t *testing.T) {
	e := newEnv(t)
	e.cfg.AllowPrivateTargets = "all"
	release := make(chan struct{})
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
	}))
	defer up.Close()
	defer close(release)
	c, ws := e.account(t, "a@example.com")
	if resp, body := addTarget(t, e, c, "slow", up.URL, "1h"); resp.StatusCode != 303 {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	waitFor10s(t, "the runner's first probe", func() bool { return hits.Load() != 0 })
	ts, _ := ws.ListTargets(t.Context())
	resp, _ := e.post(t, c, "/targets/"+itoa(ts[0].ID)+"/check", nil)
	if resp.StatusCode != 303 {
		t.Fatalf("check: %d", resp.StatusCode)
	}
	_, page := e.get(t, c, resp.Header.Get("Location"))
	if !strings.Contains(page, "A check is already running.") {
		t.Fatalf("flash missing: %s", page)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

func TestAlertTestLimitDoesNotSave(t *testing.T) {
	e := newEnv(t)
	c, ws := e.account(t, "a@example.com")
	for i := 0; i < 5; i++ {
		e.post(t, c, "/alerts/test", url.Values{"channel": {"log"}})
	}
	resp, body := e.post(t, c, "/alerts/test", url.Values{"channel": {"slack"}, "slack_url": {"https://hooks.slack.com/services/T/B/x"}})
	if resp.StatusCode != 429 || !strings.Contains(body, "Too many test messages. Wait a minute and try again. Settings were not saved.") {
		t.Fatalf("6th: %d %s", resp.StatusCode, body)
	}
	if sk := e.srv.notifyConfig(t.Context(), ws).Slack; sk != nil {
		t.Fatalf("settings saved despite the limit: %+v", sk)
	}
}
