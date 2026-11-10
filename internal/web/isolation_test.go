package web

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"uptimemon/internal/monitor"
	"uptimemon/internal/store"
)

// Markers that must never reach another account.
const (
	isoTarget = "A-target-zz"
	isoMaint  = "A-maint-zz"
	isoStatus = "A-status-zz"
	isoSecret = "A-secret"
	isoHook   = "hooks.example"
	// isoURL fails fast and offline.
	isoURL = "http://127.0.0.1:1"
)

var isoMarkers = []string{isoTarget, isoMaint, isoStatus, isoSecret, isoHook}

func leaks(s string) []string {
	var out []string
	for _, m := range isoMarkers {
		if strings.Contains(s, m) {
			out = append(out, m)
		}
	}
	return out
}

// isoA holds the ids of workspace A's data.
type isoA struct {
	target, incident, maint int64
}

func seedWorkspace(t *testing.T, e *env, ws *store.WS, name, maintTitle string) isoA {
	t.Helper()
	ctx := context.Background()
	tg, err := ws.CreateTarget(ctx, store.Target{Name: name, URL: isoURL, Interval: time.Hour, Timeout: time.Second, ExpectStatus: 200}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.InsertCheck(ctx, store.Check{TargetID: tg.ID, CheckedAt: e.clock.Add(-time.Minute), OK: false, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	inc, err := e.st.OpenIncident(ctx, tg.ID, e.clock.Add(-time.Minute), "boom")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ws.CreateMaintenance(ctx, store.Maintenance{Title: maintTitle, StartsAt: e.clock.Add(-time.Hour), EndsAt: e.clock.Add(time.Hour)}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	// Reload starts a probe in the background. Wait until it has recorded its
	// check, incident and alert so nothing changes under the snapshots.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ins, err := ws.ListIncidents(ctx, store.IncidentFilter{TargetID: tg.ID})
		if err != nil {
			t.Fatal(err)
		}
		alerted := false
		for _, in := range ins {
			alerted = alerted || in.Channels != ""
		}
		if alerted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the monitor's first probe never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return isoA{target: tg.ID, incident: inc, maint: m.ID}
}

func seedA(t *testing.T, e *env) isoA {
	t.Helper()
	a := seedWorkspace(t, e, e.opWS, isoTarget, isoMaint)
	ctx := context.Background()
	for k, v := range map[string]string{
		"notify":       `{"slack":{"webhook_url":"https://hooks.example/A-secret"}}`,
		"status_title": isoStatus,
		"status_desc":  "A-desc-zz",
	} {
		if err := e.opWS.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

type isoSnapshot struct {
	Targets   []store.Target
	Incidents []store.Incident
	Maint     []store.Maintenance
	Checks    []store.Check
	Settings  map[string]string
}

func snapshotWS(t *testing.T, ws *store.WS, a isoA) isoSnapshot {
	t.Helper()
	ctx := context.Background()
	var s isoSnapshot
	var err error
	if s.Targets, err = ws.ListTargets(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Incidents, err = ws.ListIncidents(ctx, store.IncidentFilter{}); err != nil {
		t.Fatal(err)
	}
	if s.Maint, err = ws.ListMaintenance(ctx, time.Time{}, 100); err != nil {
		t.Fatal(err)
	}
	if s.Checks, err = ws.RecentChecks(ctx, a.target, 100); err != nil {
		t.Fatal(err)
	}
	s.Settings = map[string]string{}
	for _, k := range []string{"notify", "status_title", "status_desc"} {
		v, err := ws.GetSetting(ctx, k)
		if err != nil && err != store.ErrNotFound {
			t.Fatal(err)
		}
		s.Settings[k] = v
	}
	return s
}

// isoCase is one attack from B on a workspace route.
type isoCase struct {
	query string     // appended to the URL; %T is replaced by A's target id
	form  url.Values // POST body
	sse   bool       // the route streams; checked against published events
	also  int        // one status above 499 that is the correct answer here
}

var attackForm = url.Values{
	"name": {"x"}, "url": {isoURL}, "note": {"x"}, "title": {"x"}, "duration": {"1h"},
	"description": {"x"}, "channel": {"slack"},
}

// isoCases has one entry per workspace route, keyed "METHOD pattern".
var isoCases = map[string]isoCase{
	"GET /{$}":                      {},
	"GET /events":                   {sse: true},
	"GET /targets":                  {query: "?target=%T"},
	"POST /targets":                 {form: attackForm},
	"POST /targets/test":            {form: attackForm},
	"GET /targets/{id}":             {query: "?target=%T&tab=checks"},
	"POST /targets/{id}":            {form: attackForm},
	"POST /targets/{id}/delete":     {form: attackForm},
	"POST /targets/{id}/pause":      {form: attackForm},
	"POST /targets/{id}/mute":       {form: attackForm},
	"POST /targets/{id}/check":      {form: attackForm},
	"GET /incidents":                {query: "?target=%T"},
	"POST /incidents/{id}/ack":      {form: attackForm},
	"POST /incidents/{id}/note":     {form: attackForm},
	"GET /alerts":                   {query: "?target=%T"},
	"POST /alerts":                  {form: attackForm},
	"POST /alerts/test":             {form: attackForm, also: http.StatusBadGateway}, // B has no Slack channel
	"POST /status/settings":         {form: attackForm},
	"GET /maintenance":              {query: "?target=%T"},
	"POST /maintenance":             {form: attackForm},
	"POST /maintenance/{id}/end":    {form: attackForm},
	"POST /maintenance/{id}/delete": {form: attackForm},
	"GET /api/maintenance":          {query: "?target=%T"},
	"GET /api/status":               {query: "?target=%T"},
	"GET /api/targets/{id}/checks":  {query: "?range=30d"},
	"GET /api/incidents":            {query: "?target=%T"},
}

// pathFor replaces {id} with the id of A's matching object.
func pathFor(pattern string, a isoA) string {
	id := a.target
	switch {
	case strings.HasPrefix(pattern, "/incidents/"):
		id = a.incident
	case strings.HasPrefix(pattern, "/maintenance/"):
		id = a.maint
	}
	return strings.ReplaceAll(pattern, "{id}", strconv.FormatInt(id, 10))
}

// openStreamAs opens /events with c's cookies and sends each data line on the
// returned channel. It returns once the stream preamble has arrived.
func (e *env) openStreamAs(t *testing.T, c *http.Client) <-chan string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.ts.URL+"/events", nil)
	for _, ck := range c.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events: %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "retry:") {
		t.Fatalf("no stream preamble: %q", sc.Text())
	}
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data:") {
				ch <- l
			}
		}
	}()
	return ch
}

// quiet fails if the stream delivers a data line within d.
func quiet(t *testing.T, ch <-chan string, d time.Duration) {
	t.Helper()
	select {
	case l, ok := <-ch:
		if ok {
			t.Fatalf("B received an event for A's workspace: %s", l)
		}
	case <-time.After(d):
	}
}

var (
	isoMu      sync.Mutex
	isoCovered = map[string]bool{}
)

func recordCovered(key string) {
	isoMu.Lock()
	isoCovered[key] = true
	isoMu.Unlock()
}

func TestIsolationSuite(t *testing.T) {
	t.Run("routes", func(t *testing.T) { attackRoutes(t) })
	t.Run("aggregates", testIsolationAggregates)
	t.Run("live", testIsolationLive)
	t.Run("status", testIsolationStatus)
}

// attackRoutes sends B's request for every workspace route at A's objects.
func attackRoutes(t *testing.T) {
	e := newEnv(t)
	bob, _ := e.account(t, "b@example.com")
	a := seedA(t, e)
	before := snapshotWS(t, e.opWS, a)
	if len(before.Targets) != 1 || len(before.Incidents) == 0 || len(before.Maint) != 1 || len(before.Checks) == 0 ||
		!strings.Contains(before.Settings["notify"], isoSecret) {
		t.Fatalf("A is not seeded as the suite assumes: %+v", before)
	}
	var wsRoutes []route
	for _, r := range e.srv.routeTable() {
		if r.class == classWorkspace {
			wsRoutes = append(wsRoutes, r)
		}
	}
	for _, r := range wsRoutes {
		key := r.method + " " + r.pattern
		c, ok := isoCases[key]
		if !ok {
			t.Errorf("no isolation case for %s", key)
			continue
		}
		path := pathFor(r.pattern, a) + strings.ReplaceAll(c.query, "%T", strconv.FormatInt(a.target, 10))
		hasID := strings.Contains(r.pattern, "{id}")
		if c.sse {
			ch := e.openStreamAs(t, bob)
			e.mon.Events.Publish(monitor.Event{WorkspaceID: e.opWS.ID(), Kind: "check"})
			quiet(t, ch, 200*time.Millisecond)
			recordCovered(key)
			continue
		}
		var resp *http.Response
		var body string
		if r.method == "GET" {
			resp, body = e.get(t, bob, path)
		} else {
			resp, body = e.post(t, bob, path, c.form)
		}
		recordCovered(key)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 && resp.StatusCode != c.also {
			t.Errorf("%s: unexpected status %d", key, resp.StatusCode)
		}
		if hasID && resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", r.method, path, resp.StatusCode)
		}
		if l := leaks(body + resp.Header.Get("Location")); len(l) > 0 {
			t.Errorf("%s %s (%d) leaked %v", r.method, path, resp.StatusCode, l)
		}
	}
	if after := snapshotWS(t, e.opWS, a); !reflect.DeepEqual(before, after) {
		t.Errorf("B's requests changed A's data:\nbefore %+v\nafter  %+v", before, after)
	}
}

func testIsolationAggregates(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
	seedWorkspace(t, e, bws, "B-target-zz", "B-maint-zz")
	paths := []string{"/api/status", "/api/incidents", "/api/maintenance", "/", "/incidents", "/maintenance", "/targets"}
	capture := func() map[string]string {
		out := map[string]string{}
		for _, p := range paths {
			resp, body := e.get(t, bob, p)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: %d", p, resp.StatusCode)
			}
			out[p] = body
		}
		return out
	}
	before := capture()
	if !strings.Contains(before["/api/status"], "B-target-zz") || !strings.Contains(before["/api/incidents"], "B-target-zz") {
		t.Fatal("B's own data should be visible to B")
	}
	seedA(t, e)
	after := capture()
	for _, p := range paths {
		if before[p] != after[p] {
			t.Errorf("GET %s changed after A got data:\nbefore %s\nafter  %s", p, before[p], after[p])
		}
		if l := leaks(after[p]); len(l) > 0 {
			t.Errorf("GET %s leaked %v", p, l)
		}
	}
}

func testIsolationLive(t *testing.T) {
	e := newEnv(t)
	bob, _ := e.account(t, "b@example.com")
	a := seedA(t, e)
	control := e.openStreamAs(t, e.op) // A's own stream proves events do flow
	b := e.openStreamAs(t, bob)
	e.mon.CheckAsync(e.opWS.ID(), a.target)
	if resp, body := e.post(t, e.op, "/maintenance", url.Values{"title": {"A-maint2-zz"}, "duration": {"1h"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("A maintenance: %d %s", resp.StatusCode, body)
	}
	select {
	case <-control:
	case <-time.After(5 * time.Second):
		t.Fatal("A's own stream got no event; the test would pass vacuously")
	}
	// Let the check finish publishing before judging B's stream.
	time.Sleep(300 * time.Millisecond)
	quiet(t, b, 300*time.Millisecond)
}

func testIsolationStatus(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
	seedA(t, e)
	ctx := context.Background()
	tg, err := bws.CreateTarget(ctx, store.Target{Name: "B-target-zz", URL: isoURL, Interval: time.Hour, Timeout: time.Second, ExpectStatus: 200}, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	e.mon.CheckNow(ctx, tg.ID) // refused connection: B's monitor is down
	if _, err := bws.CreateMaintenance(ctx, store.Maintenance{Title: "B-maint-zz", StartsAt: e.clock.Add(-time.Hour), EndsAt: e.clock.Add(time.Hour)}, e.clock); err != nil {
		t.Fatal(err)
	}
	// B warms the shared cache for A's page, then an anonymous visitor reads it.
	resp, warm := e.get(t, bob, "/s/default")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B on /s/default: %d", resp.StatusCode)
	}
	resp, anon := e.get(t, e.client(t), "/s/default")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous /s/default: %d", resp.StatusCode)
	}
	for name, body := range map[string]string{"B's view": warm, "anonymous": anon} {
		if !strings.Contains(body, isoTarget) {
			t.Errorf("%s: A's page should list A's monitor", name)
		}
		if strings.Contains(body, "B-target-zz") || strings.Contains(body, "B-maint-zz") {
			t.Errorf("%s: A's status page shows B's data", name)
		}
	}
	// And the other way round: B's page holds only B's data.
	resp, bpage := e.get(t, e.client(t), "/s/b")
	if resp.StatusCode != http.StatusOK || !strings.Contains(bpage, "B-target-zz") {
		t.Fatalf("B's own status page: %d", resp.StatusCode)
	}
	if l := leaks(bpage); len(l) > 0 {
		t.Errorf("B's status page leaked %v", l)
	}
}

// TestEveryWorkspaceRouteHasIsolationCase fails when a workspace route was
// not exercised by the suite, or the suite has a case for a route that is gone.
func TestEveryWorkspaceRouteHasIsolationCase(t *testing.T) {
	isoMu.Lock()
	clear(isoCovered)
	isoMu.Unlock()
	t.Run("run", func(t *testing.T) { attackRoutes(t) })
	e := newEnv(t)
	isoMu.Lock()
	defer isoMu.Unlock()
	seen := map[string]bool{}
	for _, r := range e.srv.routeTable() {
		if r.class != classWorkspace {
			continue
		}
		key := r.method + " " + r.pattern
		seen[key] = true
		if !isoCovered[key] {
			t.Errorf("workspace route %s has no isolation case", key)
		}
	}
	for key := range isoCases {
		if !seen[key] {
			t.Errorf("isolation case %q matches no workspace route", key)
		}
	}
}
