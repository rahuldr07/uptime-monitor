package web

import (
	"bufio"
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/monitor"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// Markers that must never reach another account.
const (
	isoTarget = "A-target-zz"
	isoMaint  = "A-maint-zz"
	isoStatus = "A-status-zz"
	isoSecret = "A-secret"
	isoHook   = "hooks.example"
	isoDesc   = "A-desc-zz"
	isoAlert  = "A-slackerr-zz"
	isoChat   = "A-chat-zz"
	// isoURL fails fast and offline.
	isoURL = "http://127.0.0.1:1"
)

var isoMarkers = []string{isoTarget, isoMaint, isoStatus, isoSecret, isoHook, isoDesc, isoAlert, isoChat}

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
	tg, err := ws.CreateTarget(ctx, store.Target{Name: name, URL: isoURL, Interval: time.Hour, Timeout: time.Second, ExpectStatus: 200}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	// Reload starts a probe in the background. Wait until it has recorded its
	// check, incident and alert before seeding anything else: a maintenance
	// window created earlier would make the probe open no incident, and a
	// hand-made incident created earlier would sit beside the probe's.
	deadline := time.Now().Add(15 * time.Second)
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
	if err := e.st.InsertCheck(ctx, store.Check{TargetID: tg.ID, CheckedAt: e.now().Add(-time.Minute), OK: false, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	inc, err := e.st.OpenIncident(ctx, tg.ID, e.now().Add(-time.Minute), "boom")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ws.CreateMaintenance(ctx, store.Maintenance{Title: maintTitle, StartsAt: e.now().Add(-time.Hour), EndsAt: e.now().Add(time.Hour)}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	return isoA{target: tg.ID, incident: inc, maint: m.ID}
}

func seedA(t *testing.T, e *env) isoA {
	t.Helper()
	a := seedWorkspace(t, e, e.opWS, isoTarget, isoMaint)
	ctx := context.Background()
	for k, v := range map[string]string{
		"notify":       `{"slack":{"webhook_url":"https://hooks.example/A-secret"},"telegram":{"bot_token":"A-secret-tg","chat_id":"A-chat-zz"}}`,
		"status_title": isoStatus,
		"status_desc":  isoDesc,
	} {
		if err := e.opWS.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	// A failed Slack delivery: shows on /alerts as the channel's last status.
	if err := e.st.InsertAlert(ctx, store.Alert{IncidentID: a.incident, TargetID: a.target, SentAt: e.now(),
		Channel: "slack", Event: "down", OK: false, Error: isoAlert}); err != nil {
		t.Fatal(err)
	}
	return a
}

type isoSnapshot struct {
	Targets   []store.Target
	Incidents []store.Incident
	Maint     []store.Maintenance
	Checks    []store.Check
	Settings  map[string]string
	Slug      string
	Name      string
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
	info, err := ws.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s.Slug, s.Name = info.Slug, info.Name
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

// statusAttackForm also tries to move the status page address and rename.
var statusAttackForm = func() url.Values {
	f := url.Values{"slug": {"attacker-slug"}, "name": {"Attacker"}}
	for k, v := range attackForm {
		if k != "name" {
			f[k] = v
		}
	}
	return f
}()

// isoCase is one attack from B on a workspace route.
type isoCase struct {
	query string     // appended to the URL; %T is replaced by A's target id
	form  url.Values // POST body
	sse   bool       // the route streams; checked against published events
	also  int        // one status above 499 that is the correct answer here
	want  string     // B's own answer, which the body must contain
	deny  string     // the body must not contain this (B has no such state)
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
	"GET /alerts":                   {query: "?target=%T", deny: "leave blank to keep"}, // B stored no channel
	"POST /alerts":                  {form: attackForm},
	"POST /alerts/test":             {form: attackForm, also: http.StatusBadGateway, want: "no channel named"}, // B has no Slack channel
	"POST /status/settings":         {form: statusAttackForm},
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
		if !ok {
			t.Fatal("B's stream closed; the quiet check proves nothing")
		}
		t.Fatalf("B received an event for A's workspace: %s", l)
	case <-time.After(d):
	}
}

// delivered publishes an event for B's workspace and requires it on B's
// stream, so a silent stream is known to be a working one.
func (e *env) delivered(t *testing.T, ch <-chan string, wsID int64) {
	t.Helper()
	e.mon.Events.Publish(monitor.Event{WorkspaceID: wsID, Kind: "check"})
	select {
	case l, ok := <-ch:
		if !ok {
			t.Fatal("B's stream closed")
		}
		if !strings.Contains(l, `"kind":"check"`) {
			t.Fatalf("unexpected event on B's stream: %s", l)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B's own event never arrived; the stream is not delivering")
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
	t.Run("delete", testIsolationDelete)
	t.Run("invite", testIsolationInvite)
	t.Run("slug", testIsolationSlug)
}

// testIsolationDelete: B deleting their own account removes only B.
func testIsolationDelete(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
	a := seedA(t, e)
	b := seedWorkspace(t, e, bws, "B-target-zz", "B-maint-zz")
	// A second A target on a short interval, so its runner is seen working
	// without any manual check.
	live, err := e.opWS.CreateTarget(context.Background(), store.Target{Name: "A-live", URL: isoURL, Interval: time.Second, Timeout: time.Second, ExpectStatus: 200}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(context.Background(), live.ID); err != nil {
		t.Fatal(err)
	}
	waitFor10s(t, "A's live target first check", func() bool { return e.countChecks(t, live.ID) > 0 })
	// This test stays stable only because of A's maintenance window (it
	// covers the fixed clock) and the fixed clock itself: A-live keeps probing
	// during B's deletion, and the window keeps those probes from opening
	// incidents or alerts that would change the before/after snapshot of A.
	before := snapshotWS(t, e.opWS, a)
	users := e.userCount(t)

	resp, body := e.post(t, bob, "/account/delete", url.Values{"current": {testSecret}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?") {
		t.Fatalf("B's delete: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if got := e.userCount(t); got != users-1 {
		t.Errorf("users after delete = %d, want %d", got, users-1)
	}
	if _, ok := e.mon.Get(bws.ID(), b.target); ok {
		t.Error("B's runner survived B's deletion")
	}
	if after := snapshotWS(t, e.opWS, a); !reflect.DeepEqual(before, after) {
		t.Errorf("B's deletion changed A's data:\nbefore %+v\nafter  %+v", before, after)
	}
	if _, ok := e.mon.Get(e.opWS.ID(), a.target); !ok {
		t.Fatal("A's runner was removed by B's deletion")
	}
	// A's runner keeps probing on its own interval; no manual check is made.
	n := e.countChecks(t, live.ID)
	waitFor10s(t, "A's runner to record a check after B's deletion", func() bool { return e.countChecks(t, live.ID) > n })
	e.signIn(t, opEmail, testSecret) // A can still sign in
	if resp, body := e.get(t, e.client(t), "/s/default"); resp.StatusCode != http.StatusOK || !strings.Contains(body, isoTarget) {
		t.Errorf("A's status page after B's deletion: %d", resp.StatusCode)
	}
}

// userRow reads a user's credentials, creation time and memberships as raw
// text, so any rewrite of the row shows up byte for byte.
func (e *env) userRow(t *testing.T, email string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int64
	var hash, created, op, verified, disabled string
	if err := db.QueryRow(`SELECT id, password_hash, CAST(created_at AS TEXT), CAST(is_operator AS TEXT), IFNULL(CAST(verified_at AS TEXT), 'NULL'), IFNULL(CAST(disabled_at AS TEXT), 'NULL') FROM users WHERE email = ?`, email).Scan(&id, &hash, &created, &op, &verified, &disabled); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT workspace_id, role FROM memberships WHERE user_id = ? ORDER BY workspace_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := strconv.FormatInt(id, 10) + "|" + hash + "|" + created + "|" + op + "|" + verified + "|" + disabled
	for rows.Next() {
		var ws int64
		var role string
		if err := rows.Scan(&ws, &role); err != nil {
			t.Fatal(err)
		}
		out += "|" + strconv.FormatInt(ws, 10) + ":" + role
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// testIsolationInvite: an invite bound to A's email cannot create a second
// account for that address, whoever presents it.
func testIsolationInvite(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	bob, _ := e.account(t, "b@example.com")
	a := seedA(t, e)
	before := snapshotWS(t, e.opWS, a)
	users := e.userCount(t)
	raw := e.invite(t, opEmail)
	userBefore := e.userRow(t, opEmail)
	// The attack uses a password that is not A's, so a rewrite of A's row
	// cannot hide behind a hash of the same password.
	attack := func(email string) url.Values {
		f := signupForm(raw, email)
		f.Set("password", "attacker-pw-zz")
		f.Set("confirm", "attacker-pw-zz")
		return f
	}

	// Anonymous, with A's own email: the duplicate path.
	resp, body := e.post(t, e.client(t), "/signup", attack(opEmail))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgEmailTaken) {
		t.Errorf("anonymous, A's email: %d %s", resp.StatusCode, body)
	}
	// Anonymous, with a different email: the invite is bound.
	resp, body = e.post(t, e.client(t), "/signup", attack("evil@example.com"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgInviteEmail) {
		t.Errorf("anonymous, other email: %d %s", resp.StatusCode, body)
	}
	// Signed in as B: guests only, so B is sent home and nothing is created.
	for _, em := range []string{opEmail, "evil@example.com"} {
		resp, _ = e.post(t, bob, "/signup", attack(em))
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Errorf("B signing up with %s: %d %q", em, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if got := e.userCount(t); got != users {
		t.Errorf("users = %d, want %d: a second account was created", got, users)
	}
	if after := e.userRow(t, opEmail); after != userBefore {
		t.Errorf("A's user row changed:\nbefore %s\nafter  %s", userBefore, after)
	}
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {opEmail}, "password": {"attacker-pw-zz"}}); resp.StatusCode == http.StatusSeeOther {
		t.Error("the attacker's password signs in as A")
	}
	if resp, body := e.get(t, e.signIn(t, opEmail, testSecret), "/"); resp.StatusCode != http.StatusOK || !strings.Contains(body, isoTarget) {
		t.Errorf("A's sign-in no longer lands in A's workspace: %d", resp.StatusCode)
	}
	if after := snapshotWS(t, e.opWS, a); !reflect.DeepEqual(before, after) {
		t.Errorf("sign-up attempts changed A's data:\nbefore %+v\nafter  %+v", before, after)
	}
}

// testIsolationSlug: B cannot take A's status page address.
func testIsolationSlug(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
	a := seedA(t, e)
	before := snapshotWS(t, e.opWS, a)
	if before.Slug != "default" {
		t.Fatalf("A's slug = %q, want default", before.Slug)
	}
	resp, body := e.post(t, bob, "/status/settings", url.Values{"slug": {"default"}, "name": {"Attacker"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "That address is taken.") {
		t.Fatalf("B taking default: %d %s", resp.StatusCode, body)
	}
	if after := snapshotWS(t, e.opWS, a); !reflect.DeepEqual(before, after) {
		t.Errorf("A changed:\nbefore %+v\nafter  %+v", before, after)
	}
	info, err := bws.Info(context.Background())
	if err != nil || info.Slug != "b" {
		t.Errorf("B's slug = %q, %v; want b", info.Slug, err)
	}
	resp, page := e.get(t, e.client(t), "/s/default")
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, isoTarget) || strings.Contains(page, "Attacker") {
		t.Errorf("A's page: %d", resp.StatusCode)
	}
}

// attackRoutes sends B's request for every workspace route at A's objects.
func attackRoutes(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
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
			e.delivered(t, ch, bws.ID())
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
		if c.want != "" && !strings.Contains(body, c.want) {
			t.Errorf("%s: body lacks %q, so B's own state was not used", key, c.want)
		}
		if c.deny != "" && strings.Contains(body, c.deny) {
			t.Errorf("%s: body contains %q: A's state reached B", key, c.deny)
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
	paths := []string{"/api/status", "/api/incidents", "/api/maintenance", "/", "/alerts", "/incidents", "/maintenance", "/targets"}
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
			t.Errorf("GET %s changed after A got data; first difference:\n%s", p, firstDiff(before[p], after[p]))
		}
		if l := leaks(after[p]); len(l) > 0 {
			t.Errorf("GET %s leaked %v", p, l)
		}
	}
}

func testIsolationLive(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
	a := seedA(t, e)
	control := e.openStreamAs(t, e.op) // A's own stream proves events do flow
	b := e.openStreamAs(t, bob)
	e.mon.CheckAsync(e.opWS.ID(), a.target)
	if resp, body := e.post(t, e.op, "/maintenance", url.Values{"title": {"A-maint2-zz"}, "duration": {"1h"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("A maintenance: %d %s", resp.StatusCode, body)
	}
	select {
	case <-control:
	case <-time.After(10 * time.Second):
		t.Fatal("A's own stream got no event; the test would pass vacuously")
	}
	// Let the check finish publishing before judging B's stream.
	waitFor10s(t, "A's check result", func() bool {
		st, ok := e.mon.Get(e.opWS.ID(), a.target)
		return ok && st.Last != nil
	})
	quiet(t, b, 300*time.Millisecond)
	e.delivered(t, b, bws.ID())
}

func testIsolationStatus(t *testing.T) {
	e := newEnv(t)
	bob, bws := e.account(t, "b@example.com")
	seedA(t, e)
	ctx := context.Background()
	tg, err := bws.CreateTarget(ctx, store.Target{Name: "B-target-zz", URL: isoURL, Interval: time.Hour, Timeout: time.Second, ExpectStatus: 200}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	e.mon.CheckNow(ctx, tg.ID) // refused connection: B's monitor is down
	waitFor10s(t, "B's monitor to be down", func() bool {
		st, ok := e.mon.Get(bws.ID(), tg.ID)
		return ok && st.Status == monitor.Down
	})
	if _, err := bws.CreateMaintenance(ctx, store.Maintenance{Title: "B-maint-zz", StartsAt: e.now().Add(-time.Hour), EndsAt: e.now().Add(time.Hour)}, e.now()); err != nil {
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
		for _, want := range []string{isoTarget, isoStatus, isoDesc} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: A's page should contain %q", name, want)
			}
		}
		if strings.Contains(body, "B-target-zz") || strings.Contains(body, "B-maint-zz") {
			t.Errorf("%s: A's status page shows B's data", name)
		}
	}
	// And the other way round: B's page holds only B's data.
	resp, bpage := e.get(t, e.client(t), "/s/b")
	if resp.StatusCode != http.StatusOK || !strings.Contains(bpage, "B-target-zz") || !strings.Contains(bpage, "B-maint-zz") {
		t.Fatalf("B's own status page should show its monitor and active maintenance: %d", resp.StatusCode)
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

// firstDiff shows the first line where two bodies differ.
func firstDiff(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(al) && i < len(bl); i++ {
		if al[i] != bl[i] {
			return "before: " + al[i] + "\nafter:  " + bl[i]
		}
	}
	return "lengths differ: " + strconv.Itoa(len(al)) + " vs " + strconv.Itoa(len(bl)) + " lines"
}
