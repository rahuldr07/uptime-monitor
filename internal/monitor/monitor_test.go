package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rahuldr07/uptime-monitor/internal/mail"
	"github.com/rahuldr07/uptime-monitor/internal/notify"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

type recorder struct {
	mu     sync.Mutex
	events []notify.Event
	fail   int32 // number of calls to fail before succeeding
}

func (r *recorder) Notify(_ context.Context, ev notify.Event) error {
	if atomic.LoadInt32(&r.fail) > 0 {
		atomic.AddInt32(&r.fail, -1)
		return errors.New("transient")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *recorder) list() []notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notify.Event(nil), r.events...)
}

func allowAll(int64) bool { return true }

type fixture struct {
	st     *store.Store
	mon    *Monitor
	rec    *recorder
	target store.Target
	status int32
	body   string
	clk    atomic.Int64 // UnixNano; use now, advance, setClock
	srv    *httptest.Server
}

func (f *fixture) now() time.Time          { return time.Unix(0, f.clk.Load()).UTC() }
func (f *fixture) setClock(t time.Time)    { f.clk.Store(t.UnixNano()) }
func (f *fixture) advance(d time.Duration) { f.clk.Add(int64(d)) }

func setup(t *testing.T, threshold int) *fixture {
	t.Helper()
	f := &fixture{status: 200, body: "hello"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(atomic.LoadInt32(&f.status)))
		w.Write([]byte(f.body))
	}))
	t.Cleanup(f.srv.Close)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f.st = st
	f.setClock(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC))
	f.target, _ = st.Workspace(1).CreateTarget(context.Background(), store.Target{Name: "api", URL: f.srv.URL, Interval: time.Hour, Timeout: 2 * time.Second, Keyword: "hello"}, f.now())
	f.rec = &recorder{}
	f.mon = New(Options{Store: st, Notifiers: func(context.Context, int64) (notify.Multi, error) {
		return notify.Multi{{Name: "rec", Notifier: f.rec}}, nil
	}, Logger: slog.Default(),
		FailureThreshold: threshold, Client: f.srv.Client(), Now: f.now, RetryBase: time.Millisecond})
	return f
}

func (f *fixture) tick(t *testing.T) State {
	t.Helper()
	f.advance(30 * time.Second)
	f.mon.CheckNow(context.Background(), f.target.ID)
	f.mon.WaitAlerts()
	s, _ := f.mon.Get(1, f.target.ID)
	return s
}

func TestStateMachineWithThreshold(t *testing.T) {
	f := setup(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Start with no runner context so ticks are manual.
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}

	if s := f.tick(t); s.Status != Up || s.Last == nil || !s.Last.OK {
		t.Fatalf("first ok check should be Up: %+v", s)
	}
	atomic.StoreInt32(&f.status, 503)
	if s := f.tick(t); s.Status != Up || s.Failures != 1 {
		t.Fatalf("one failure should not flip with threshold 2: %+v", s)
	}
	if len(f.rec.list()) != 0 {
		t.Fatal("no alert expected yet")
	}
	s := f.tick(t)
	if s.Status != Down || s.IncidentID == 0 {
		t.Fatalf("second failure should be Down with incident: %+v", s)
	}
	if !s.Since.Equal(f.now().Add(-30 * time.Second)) {
		t.Errorf("down since should be the first failure, got %v", s.Since)
	}
	evs := f.rec.list()
	if len(evs) != 1 || evs[0].Up || evs[0].StatusCode != 503 {
		t.Fatalf("expected one down alert: %+v", evs)
	}
	in, err := f.st.OngoingIncident(ctx, f.target.ID)
	if err != nil || in.Cause != "HTTP 503" || in.Channels != "rec" {
		t.Fatalf("incident: %v %+v", err, in)
	}
	f.tick(t)
	if len(f.rec.list()) != 1 {
		t.Fatal("still down should not re-alert")
	}
	atomic.StoreInt32(&f.status, 200)
	s = f.tick(t)
	if s.Status != Up || s.IncidentID != 0 {
		t.Fatalf("recovery should be immediate: %+v", s)
	}
	evs = f.rec.list()
	if len(evs) != 2 || !evs[1].Up || evs[1].DownFor != 90*time.Second {
		t.Fatalf("expected up alert with DownFor=90s: %+v", evs)
	}
	if _, err := f.st.OngoingIncident(ctx, f.target.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("incident should be closed: %v", err)
	}
	if n, _ := f.st.Workspace(1).CountAlerts(ctx, time.Time{}); n != 2 {
		t.Fatalf("alerts recorded: %d", n)
	}
}

func TestKeywordAndExpectStatus(t *testing.T) {
	f := setup(t, 1)
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	f.body = "goodbye"
	if s := f.tick(t); s.Status != Down || s.Last.Error == "" {
		t.Fatalf("missing keyword should fail: %+v", s)
	}
	f.body = "hello"
	f.tick(t)
	f.target.ExpectStatus = 204
	f.mon.states[f.target.ID].Target = f.target
	if s := f.tick(t); s.Status != Down || s.Last.Error != "expected HTTP 204, got 200" {
		t.Fatalf("expect status: %+v", s.Last)
	}
}

func TestRestoreFromStore(t *testing.T) {
	f := setup(t, 1)
	ctx := context.Background()
	id, _ := f.st.OpenIncident(ctx, f.target.ID, f.now().Add(-time.Hour), "HTTP 500")
	f.st.InsertCheck(ctx, store.Check{TargetID: f.target.ID, CheckedAt: f.now().Add(-time.Minute), OK: false, StatusCode: 500})
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	s, _ := f.mon.Get(1, f.target.ID)
	if s.Status != Down || s.IncidentID != id || !s.Since.Equal(f.now().Add(-time.Hour)) {
		t.Fatalf("restore: %+v", s)
	}
	// Target comes back up: closes the restored incident, alerts once, no down alert.
	s = f.tick(t)
	if s.Status != Up {
		t.Fatalf("want up: %+v", s)
	}
	evs := f.rec.list()
	if len(evs) != 1 || !evs[0].Up || evs[0].DownFor != time.Hour+30*time.Second {
		t.Fatalf("expected single recovery alert: %+v", evs)
	}
}

func TestAlertRetries(t *testing.T) {
	f := setup(t, 1)
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	atomic.StoreInt32(&f.rec.fail, 2) // fail twice, succeed on third
	atomic.StoreInt32(&f.status, 500)
	f.tick(t)
	if len(f.rec.list()) != 1 {
		t.Fatalf("expected alert after retries: %+v", f.rec.list())
	}
	in, _ := f.st.OngoingIncident(context.Background(), f.target.ID)
	if in.Channels != "rec" {
		t.Fatalf("channel should be recorded as ok: %+v", in)
	}
}

func TestPauseResumeAndRemove(t *testing.T) {
	f := setup(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, _ := f.mon.Get(1, f.target.ID); s.Status == Up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner never checked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.st.Workspace(1).SetPaused(ctx, f.target.ID, true)
	f.mon.Reload(ctx, f.target.ID)
	if s, _ := f.mon.Get(1, f.target.ID); s.Status != Paused {
		t.Fatalf("want paused: %+v", s)
	}
	f.st.Workspace(1).SetPaused(ctx, f.target.ID, false)
	f.mon.Reload(ctx, f.target.ID)
	if s, _ := f.mon.Get(1, f.target.ID); s.Status == Paused {
		t.Fatalf("want resumed: %+v", s)
	}
	f.mon.Remove(f.target.ID)
	if _, ok := f.mon.Get(1, f.target.ID); ok {
		t.Fatal("removed target still present")
	}
	cancel()
	f.mon.Wait()
}

func TestProbeTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	c := Probe(context.Background(), slow.Client(), store.Target{URL: slow.URL, Timeout: 50 * time.Millisecond}, time.Now)
	if c.OK || c.Error != "timeout after 50ms" {
		t.Fatalf("probe: %+v", c)
	}
}

func TestPauseClosesIncidentAndResumeIsClean(t *testing.T) {
	f := setup(t, 1)
	ctx := context.Background()
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	atomic.StoreInt32(&f.status, 500)
	s := f.tick(t)
	if s.Status != Down || s.IncidentID == 0 {
		t.Fatalf("want down with incident: %+v", s)
	}
	in, _ := f.st.OngoingIncident(ctx, f.target.ID)
	if !in.StartedAt.Equal(s.Since) {
		t.Fatalf("incident start %v should equal Since %v", in.StartedAt, s.Since)
	}
	f.st.Workspace(1).SetPaused(ctx, f.target.ID, true)
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.OngoingIncident(ctx, f.target.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pausing should close the incident: %v", err)
	}
	f.st.Workspace(1).SetPaused(ctx, f.target.ID, false)
	f.mon.Reload(ctx, f.target.ID)
	s, _ = f.mon.Get(1, f.target.ID)
	if s.Status != Unknown || s.IncidentID != 0 || s.Failures != 0 {
		t.Fatalf("resume should reset state: %+v", s)
	}
	// Still failing after resume: a fresh incident starting now, not before the pause.
	before := len(f.rec.list())
	s = f.tick(t)
	if s.Status != Down || !s.Since.Equal(f.now()) {
		t.Fatalf("new incident should start at the post-resume failure: %+v (clock %v)", s, f.now())
	}
	if list, _ := f.st.Workspace(1).ListIncidents(ctx, store.IncidentFilter{OngoingOnly: true}); len(list) != 1 {
		t.Fatalf("exactly one ongoing incident, got %d", len(list))
	}
	if len(f.rec.list()) != before+1 {
		t.Fatal("expected a new down alert after resume")
	}
	atomic.StoreInt32(&f.status, 200)
	if s = f.tick(t); s.Status != Up {
		t.Fatalf("want up: %+v", s)
	}
	if list, _ := f.st.Workspace(1).ListIncidents(ctx, store.IncidentFilter{OngoingOnly: true}); len(list) != 0 {
		t.Fatalf("no ongoing incidents expected, got %d", len(list))
	}
}

func TestConcurrentCheckNowIsSerialised(t *testing.T) {
	f := setup(t, 1)
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				atomic.StoreInt32(&f.status, 500)
			} else {
				atomic.StoreInt32(&f.status, 200)
			}
			// checkClaimed, not CheckNow: the busy flag would turn most
			// of these calls into no-ops and leave checkMu untested.
			f.mon.checkClaimed(context.Background(), f.target.ID, f.mon.states[f.target.ID])
		}(i)
	}
	wg.Wait()
	f.mon.WaitAlerts()
	list, _ := f.st.Workspace(1).ListIncidents(context.Background(), store.IncidentFilter{OngoingOnly: true})
	s, _ := f.mon.Get(1, f.target.ID)
	if (s.Status == Down) != (len(list) == 1) || len(list) > 1 {
		t.Fatalf("state %v but %d ongoing incidents", s.Status, len(list))
	}
}

func TestPauseWaitsForInFlightCheck(t *testing.T) {
	f := setup(t, 1)
	ctx := context.Background()
	// Slow server: the probe holds checkMu for a while.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(500)
	}))
	defer slow.Close()
	f.target.URL = slow.URL
	f.target.Timeout = 2 * time.Second
	f.mon.client = slow.Client()
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	done := make(chan struct{})
	go func() { f.mon.CheckNow(ctx, f.target.ID); close(done) }()
	time.Sleep(50 * time.Millisecond) // probe is in flight
	f.st.Workspace(1).SetPaused(ctx, f.target.ID, true)
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	<-done
	f.mon.WaitAlerts()
	// Either the check applied first and pause closed the incident, or pause
	// won and the check was dropped: in both cases nothing stays open.
	if list, _ := f.st.Workspace(1).ListIncidents(ctx, store.IncidentFilter{OngoingOnly: true}); len(list) != 0 {
		t.Fatalf("incident left open across pause: %+v", list)
	}
	if s, _ := f.mon.Get(1, f.target.ID); s.Status != Paused || s.IncidentID != 0 {
		t.Fatalf("state after pause: %+v", s)
	}
}

func TestMaintenanceSuppressesIncidentsAndAlerts(t *testing.T) {
	f := setup(t, 2)
	ctx := context.Background()
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}} // manual ticks, no runner
	if s := f.tick(t); s.Status != Up {
		t.Fatalf("want up, got %v", s.Status)
	}
	// A window covering the next hour, for every target.
	if _, err := f.st.Workspace(1).CreateMaintenance(ctx, store.Maintenance{Title: "deploy", StartsAt: f.now(), EndsAt: f.now().Add(time.Hour)}, f.now()); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt32(&f.status, 500)
	for i := 0; i < 3; i++ {
		s := f.tick(t)
		if s.Status != Up || s.Failures != 0 || s.IncidentID != 0 {
			t.Fatalf("tick %d during maintenance: status %v failures %d incident %d", i, s.Status, s.Failures, s.IncidentID)
		}
		if s.Last == nil || s.Last.OK || !s.Last.Maintenance {
			t.Fatalf("last check should be a flagged failure: %+v", s.Last)
		}
	}
	if got := f.rec.list(); len(got) != 0 {
		t.Fatalf("no alerts expected during maintenance, got %+v", got)
	}
	if list, _ := f.st.Workspace(1).ListIncidents(ctx, store.IncidentFilter{}); len(list) != 0 {
		t.Fatalf("no incidents expected during maintenance, got %+v", list)
	}
	// Window over: failures count again from zero, so threshold 2 needs two ticks.
	f.advance(time.Hour)
	if s := f.tick(t); s.Status != Up || s.Failures != 1 {
		t.Fatalf("first failure after window: %v %d", s.Status, s.Failures)
	}
	s := f.tick(t)
	if s.Status != Down || s.IncidentID == 0 || s.Last.Maintenance {
		t.Fatalf("second failure after window should open an incident: %+v", s)
	}
	if got := f.rec.list(); len(got) != 1 || got[0].Up {
		t.Fatalf("want one down alert, got %+v", got)
	}
	// A recovery seen inside a later window still closes the incident.
	f.st.Workspace(1).CreateMaintenance(ctx, store.Maintenance{Title: "fix", StartsAt: f.now(), EndsAt: f.now().Add(time.Hour), Group: ""}, f.now())
	atomic.StoreInt32(&f.status, 200)
	if s := f.tick(t); s.Status != Up || s.IncidentID != 0 {
		t.Fatalf("recovery during maintenance: %+v", s)
	}
	if got := f.rec.list(); len(got) != 2 || !got[1].Up {
		t.Fatalf("want an up alert, got %+v", got)
	}
}

// blocker is a channel that hangs until released, like a stalled SMTP server.
type blocker struct{ release chan struct{} }

func (b blocker) Notify(ctx context.Context, _ notify.Event) error {
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSlowChannelDoesNotDelayOthers(t *testing.T) {
	f := setup(t, 1)
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	slow := blocker{release: make(chan struct{})}
	f.mon.SetNotifiers(1, notify.Multi{{Name: "slow", Notifier: slow}, {Name: "rec", Notifier: f.rec}})
	atomic.StoreInt32(&f.status, 503)
	f.advance(30 * time.Second)
	f.mon.CheckNow(context.Background(), f.target.ID)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.rec.list()) == 0 {
		if time.Now().After(deadline) {
			close(slow.release)
			t.Fatal("fast channel was held up by the slow one")
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(slow.release)
	f.mon.WaitAlerts()
	in, err := f.st.OngoingIncident(context.Background(), f.target.ID)
	if err != nil || in.Channels != "rec,slow" && in.Channels != "slow,rec" {
		t.Fatalf("both deliveries should be recorded: %v %q", err, in.Channels)
	}
}

func TestProbeBlockedHidesAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	tg := store.Target{URL: srv.URL, Timeout: 2 * time.Second}
	c := New(Options{Store: st}).Test(context.Background(), 1, tg)
	if c.OK || c.Error != "blocked: destination is an internal address" {
		t.Fatalf("want blocked, got %+v", c)
	}
	if strings.Contains(c.Error, "127.0.0.1") {
		t.Fatal("error leaks address")
	}
	if c := New(Options{Store: st, PrivateAllowed: allowAll}).Test(context.Background(), 1, tg); !c.OK {
		t.Fatalf("want ok with AllowPrivate, got %+v", c)
	}
}

func TestProbeStaysOnHTTP1AgainstHTTP2Server(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Proto))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(Options{Store: st, PrivateAllowed: allowAll})
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	gtr := m.client.Transport.(*http.Transport)
	if gtr.ForceAttemptHTTP2 || (gtr.TLSClientConfig != nil && gtr.TLSClientConfig.NextProtos != nil) {
		t.Fatalf("guarded client must stay on HTTP/1.1: %+v", gtr)
	}
	tr := m.openClient.Transport.(*http.Transport)
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.RootCAs = pool
	c := m.Test(context.Background(), 1, store.Target{URL: srv.URL, Timeout: 5 * time.Second, Keyword: "HTTP/1.1"})
	if !c.OK {
		t.Fatalf("want ok over HTTP/1.1, got %+v", c)
	}
}

// twoWorkspaces opens a store with workspace 1 and a second workspace, and
// creates one target in each pointing at url.
func twoWorkspaces(t *testing.T, url string) (*store.Store, store.Target, store.Target) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ws.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO workspaces (name, slug, created_at) VALUES ('B','b',0)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	mk := func(ws int64, name string) store.Target {
		tg, err := st.Workspace(ws).CreateTarget(ctx, store.Target{Name: name, URL: url, Interval: time.Hour, Timeout: 2 * time.Second}, now)
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}
	return st, mk(1, "one"), mk(2, "two")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMonitorScopedByWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	st, t1, t2 := twoWorkspaces(t, srv.URL)
	m := New(Options{Store: st, Client: srv.Client(), Logger: slog.Default()})
	evs, unsub := m.Events.Subscribe()
	defer unsub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both checked", func() bool {
		a, _ := m.Get(1, t1.ID)
		b, _ := m.Get(2, t2.ID)
		return a.Last != nil && b.Last != nil
	})
	if s := m.Snapshot(1); len(s) != 1 || s[0].Target.ID != t1.ID {
		t.Fatalf("Snapshot(1): %+v", s)
	}
	if s := m.Snapshot(2); len(s) != 1 || s[0].Target.ID != t2.ID {
		t.Fatalf("Snapshot(2): %+v", s)
	}
	if _, ok := m.Get(2, t1.ID); ok {
		t.Fatal("Get(2, ws1 target) should be false")
	}
	count := func(id int64) int {
		ws := int64(1)
		if id == t2.ID {
			ws = 2
		}
		cs, err := st.Workspace(ws).RecentChecks(ctx, id, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(cs)
	}
	before, before2 := count(t1.ID), count(t2.ID)
	m.CheckAsync(2, t1.ID)
	m.CheckAsync(1, t2.ID)
	time.Sleep(200 * time.Millisecond) // a wrongly accepted call would have probed by now
	if after := count(t1.ID); after != before {
		t.Fatalf("CheckAsync(2, ws1 target) recorded checks: %d -> %d", before, after)
	}
	if after := count(t2.ID); after != before2 {
		t.Fatalf("CheckAsync(1, ws2 target) recorded checks: %d -> %d", before2, after)
	}
	m.CheckAsync(2, t2.ID) // legitimate check proves the scoped path still works
	waitFor(t, "ws2 check", func() bool { return count(t2.ID) > before2 })
	want := map[int64]int64{t1.ID: 1, t2.ID: 2}
	seen := map[int64]int{}
	deadline := time.After(5 * time.Second)
	for len(seen) < len(want) {
		select {
		case ev := <-evs:
			w, ok := want[ev.TargetID]
			if !ok {
				continue
			}
			if ev.WorkspaceID != w {
				t.Fatalf("event %+v has wrong workspace, want %d", ev, w)
			}
			seen[ev.TargetID]++
		case <-deadline:
			t.Fatalf("did not see events for both targets: %v", seen)
		}
	}
	cancel()
	m.Wait()
}

func TestNotifiersPerWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	st, _, _ := twoWorkspaces(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	recs := map[int64]*recorder{1: {}, 2: {}}
	var calls3 int32
	m := New(Options{Store: st, Client: srv.Client(), Logger: slog.Default(), RetryBase: time.Millisecond,
		FailureThreshold: 1,
		Notifiers: func(_ context.Context, ws int64) (notify.Multi, error) {
			if ws == 3 {
				atomic.AddInt32(&calls3, 1)
				return nil, errors.New("boom")
			}
			return notify.Multi{{Name: "ws" + string(rune('0'+ws)), Notifier: recs[ws]}}, nil
		}})
	t3 := store.Target{ID: 99, WorkspaceID: 3, Name: "three", URL: srv.URL, Timeout: time.Second}
	m.states[t3.ID] = &state{State: State{Target: t3}}
	// Targets 1 and 2 go down (server returns 500).
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); m.Wait() }()
	waitFor(t, "alerts", func() bool { return len(recs[2].list()) == 1 && len(recs[1].list()) == 1 })
	if got := m.Notifiers(ctx, 2); len(got) != 1 || got[0].Name != "ws2" {
		t.Fatalf("ws2 notifiers: %+v", got)
	}
	other := &recorder{}
	m.SetNotifiers(2, notify.Multi{{Name: "other", Notifier: other}})
	if got := m.Notifiers(ctx, 2); got[0].Name != "other" {
		t.Fatalf("SetNotifiers did not replace: %+v", got)
	}
	if got := m.Notifiers(ctx, 1); got[0].Name != "ws1" {
		t.Fatalf("SetNotifiers(2) changed ws1: %+v", got)
	}
	// A failing loader falls back to the log channel and is retried next time.
	m.CheckNow(ctx, t3.ID)
	m.WaitAlerts()
	if n := atomic.LoadInt32(&calls3); n != 1 {
		t.Fatalf("loader calls after first alert: %d", n)
	}
	if got := m.Notifiers(ctx, 3); len(got) != 1 || got[0].Name != "log" {
		t.Fatalf("fallback: %+v", got)
	}
	if n := atomic.LoadInt32(&calls3); n != 2 {
		t.Fatalf("loader should be retried, calls: %d", n)
	}
}

func TestPrivateAllowedPerWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(Options{Store: st, PrivateAllowed: func(ws int64) bool { return ws == 1 }})
	tg := store.Target{URL: srv.URL, Timeout: 2 * time.Second}
	if c := m.Test(context.Background(), 1, tg); !c.OK {
		t.Fatalf("ws1 should be allowed: %+v", c)
	}
	if c := m.Test(context.Background(), 2, tg); c.Error != "blocked: destination is an internal address" {
		t.Fatalf("ws2 should be blocked: %+v", c)
	}
}

func TestCheckAsyncCoalesces(t *testing.T) {
	f := setup(t, 1)
	release := make(chan struct{})
	var hits atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
	}))
	defer slow.Close()
	f.target.URL = slow.URL
	f.target.Keyword = ""
	f.mon.client, f.mon.openClient = slow.Client(), slow.Client()
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	ws := f.target.WorkspaceID
	if !f.mon.CheckAsync(ws, f.target.ID) {
		t.Fatal("first CheckAsync refused")
	}
	waitFor(t, "probe to start", func() bool { return hits.Load() == 1 })
	if f.mon.CheckAsync(ws, f.target.ID) {
		t.Fatal("second CheckAsync should coalesce")
	}
	if f.mon.CheckAsync(ws+1, f.target.ID) {
		t.Fatal("foreign workspace should be refused")
	}
	close(release)
	f.mon.wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
	if !f.mon.CheckAsync(ws, f.target.ID) {
		t.Fatal("CheckAsync after completion refused")
	}
	f.mon.wg.Wait()
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
}

func TestUserAgent(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("User-Agent"))
	}))
	defer srv.Close()
	tg := store.Target{URL: srv.URL, Timeout: time.Second}
	m := New(Options{Client: srv.Client(), UserAgent: "uptime-monitor/dev (+https://status.example.com)"})
	m.Test(context.Background(), 2, tg)
	if got.Load() != "uptime-monitor/dev (+https://status.example.com)" {
		t.Errorf("User-Agent = %v", got.Load())
	}
	Probe(context.Background(), srv.Client(), tg, time.Now)
	if got.Load() != "uptime-monitor/1.0" {
		t.Errorf("default User-Agent = %v", got.Load())
	}
}

func TestCheckAsyncRefusedDuringRunnerProbe(t *testing.T) {
	f := setup(t, 1)
	release := make(chan struct{})
	var hits atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-release
	}))
	defer slow.Close()
	f.target.URL, f.target.Keyword = slow.URL, ""
	f.mon.client, f.mon.openClient = slow.Client(), slow.Client()
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	done := make(chan struct{})
	go func() { // what the runner does on a tick
		f.mon.CheckNow(context.Background(), f.target.ID)
		close(done)
	}()
	waitFor(t, "runner probe", func() bool { return hits.Load() == 1 })
	if f.mon.CheckAsync(f.target.WorkspaceID, f.target.ID) {
		t.Fatal("CheckAsync accepted during a runner probe")
	}
	// A runner tick that finds a check running skips its probe.
	f.mon.CheckNow(context.Background(), f.target.ID)
	close(release)
	<-done
	if hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", hits.Load())
	}
}

func TestReloadDoesNotReprobeFreshTarget(t *testing.T) {
	f := setup(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ws := f.st.Workspace(1)
	count := func() int {
		cs, _ := ws.RecentChecks(ctx, f.target.ID, 100)
		return len(cs)
	}
	waitFor(t, "first probe", func() bool { return count() == 1 })
	for i := 0; i < 5; i++ {
		if err := ws.SetMuted(ctx, f.target.ID, i%2 == 0); err != nil {
			t.Fatal(err)
		}
		if err := f.mon.Reload(ctx, f.target.ID); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := count(); n > 2 {
		t.Fatalf("%d checks after 5 mute toggles; want at most one extra", n)
	}
}

// An edit that changes what is probed must probe at once, even when the last
// check is still fresh; the clock does not move and CheckNow is not called.
func TestReloadProbesAtOnceWhenProbeConfigChanged(t *testing.T) {
	f := setup(t, 1)
	f.mon.reprobeGap = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ws := f.st.Workspace(1)
	count := func() int {
		cs, _ := ws.RecentChecks(ctx, f.target.ID, 100)
		return len(cs)
	}
	waitFor(t, "first probe", func() bool { return count() == 1 })
	// Unchanged probe config (a rename): still skipped.
	cur, _ := f.st.TargetByID(ctx, f.target.ID)
	cur.Name = "renamed"
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("%d checks after a rename; want 1", n)
	}
	// Changed keyword: probes without CheckNow or a clock change.
	cur.Keyword = "nope"
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probe after edit", func() bool { return count() == 2 })
	// Changed URL as well.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) }))
	defer other.Close()
	cur.URL = other.URL
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probe after URL edit", func() bool { return count() == 3 })
}

// A second Reload that cancels the probe an edit triggered must not lose the
// pending probe: the new runner still probes at once.
func TestReprobeSurvivesCancelledProbe(t *testing.T) {
	f := setup(t, 1)
	f.mon.reprobeGap = 50 * time.Millisecond
	var block atomic.Bool
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Swap(false) {
			entered <- struct{}{}
			<-r.Context().Done()
			return
		}
		w.Write([]byte("hello"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := f.st.Workspace(1)
	cur, _ := f.st.TargetByID(ctx, f.target.ID)
	cur.URL = srv.URL
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		cs, _ := ws.RecentChecks(ctx, f.target.ID, 100)
		return len(cs)
	}
	waitFor(t, "first probe", func() bool { return count() == 1 })
	block.Store(true)
	cur.Keyword = ""
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	<-entered // the edit's probe is in flight
	if err := ws.SetMuted(ctx, f.target.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probe after the second reload", func() bool { return count() == 2 })
}

func TestRemoveWorkspaceWaitsOnlyForItsOwnAlerts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	st, t1, t2 := twoWorkspaces(t, srv.URL)
	m := New(Options{Store: st, Client: srv.Client(), Logger: slog.Default(), RetryBase: time.Millisecond})
	slow1, slow2 := blocker{release: make(chan struct{})}, blocker{release: make(chan struct{})}
	m.SetNotifiers(1, notify.Multi{{Name: "slow", Notifier: slow1}})
	m.SetNotifiers(2, notify.Multi{{Name: "slow", Notifier: slow2}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "both down", func() bool {
		a, _ := m.Get(1, t1.ID)
		b, _ := m.Get(2, t2.ID)
		return a.Status == Down && b.Status == Down
	})
	done := make(chan struct{})
	go func() { m.RemoveWorkspace(2); close(done) }()
	select {
	case <-done:
		t.Fatal("RemoveWorkspace returned while its alert was still being delivered")
	case <-time.After(200 * time.Millisecond):
	}
	close(slow2.release) // workspace 1's alert stays blocked and must not matter
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveWorkspace waited on another workspace")
	}
	if _, ok := m.Get(2, t2.ID); ok {
		t.Error("removed workspace still has a target")
	}
	if _, ok := m.Get(1, t1.ID); !ok {
		t.Error("other workspace lost its target")
	}
	close(slow1.release)
	cancel()
	m.Wait()
}

// ForgetWorkspace landing between Reload's two lock sections must not leave
// a runner behind.
func TestReloadRacingForgetWorkspaceStartsNoRunner(t *testing.T) {
	f := setup(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f.mon.RemoveWorkspace(1)
	// Reload holds the target's checkMu here, so ForgetWorkspace's Remove must
	// wait for it; the hook only waits until the tombstone is set.
	forgotten := make(chan struct{})
	f.mon.afterReloadCheck = func() {
		go func() { f.mon.ForgetWorkspace(1); close(forgotten) }()
		waitFor(t, "tombstone", func() bool {
			f.mon.mu.Lock()
			defer f.mon.mu.Unlock()
			return f.mon.gone[1]
		})
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	f.mon.afterReloadCheck = nil
	<-forgotten
	if _, ok := f.mon.Get(1, f.target.ID); ok {
		t.Fatal("state of a forgotten workspace is still tracked")
	}
	f.mon.mu.Lock()
	n := len(f.mon.cancels)
	f.mon.mu.Unlock()
	if n != 0 {
		t.Fatalf("cancels = %d, want 0", n)
	}
	cancel()
	done := make(chan struct{})
	go func() { f.mon.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a runner goroutine outlived the context")
	}
}

// Two Reloads of one target at once must leave one runner: the second
// cancels the first instead of overwriting its cancel func.
func TestConcurrentReloadsLeaveOneRunner(t *testing.T) {
	f := setup(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.mon.Reload(ctx, f.target.ID); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f.mon.mu.Lock()
	n := len(f.mon.cancels)
	f.mon.mu.Unlock()
	if n != 1 {
		t.Fatalf("cancels = %d, want 1", n)
	}
	// Only the stored runner is left running: Wait returns once ctx ends only
	// if every replaced runner was cancelled too.
	f.mon.Remove(f.target.ID)
	done := make(chan struct{})
	go func() { f.mon.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a replaced runner was never cancelled")
	}
}

// Rapid probe-config edits must not flood probes: the new runner waits for
// reprobeGap after the last check, and a later Reload replaces the earlier one.
func TestRapidProbeEditsProbeOnce(t *testing.T) {
	f := setup(t, 1)
	f.mon.reprobeGap = 400 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ws := f.st.Workspace(1)
	count := func() int {
		cs, _ := ws.RecentChecks(ctx, f.target.ID, 100)
		return len(cs)
	}
	waitFor(t, "first probe", func() bool { return count() == 1 })
	cur, _ := f.st.TargetByID(ctx, f.target.ID)
	for i := 0; i < 10; i++ {
		cur.Keyword = fmt.Sprintf("kw%d", i)
		if _, err := ws.UpdateTarget(ctx, cur); err != nil {
			t.Fatal(err)
		}
		if err := f.mon.Reload(ctx, f.target.ID); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("%d checks inside the gap; want 1", n)
	}
	waitFor(t, "one probe after the gap", func() bool { return count() == 2 })
	time.Sleep(600 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("%d checks after ten edits; want 2", n)
	}
}

// A runner restarted with a nearly-due last check probes when the interval
// since that check elapses, not a whole interval after the restart.
func TestRestartedRunnerProbesWhenDue(t *testing.T) {
	f := setup(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := f.st.Workspace(1)
	// Interval is one hour; the last check is 300ms short of that.
	if err := f.st.InsertCheck(ctx, store.Check{TargetID: f.target.ID, CheckedAt: f.now().Add(-time.Hour + 300*time.Millisecond), OK: true, StatusCode: 200}); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		cs, _ := ws.RecentChecks(ctx, f.target.ID, 100)
		return len(cs)
	}
	time.Sleep(100 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("%d checks right after the restart; want 1", n)
	}
	waitFor(t, "probe when due", func() bool { return count() == 2 })
}

// Timeout and expect_status edits change what is probed, like URL and keyword.
func TestReprobeOnTimeoutAndExpectStatusChange(t *testing.T) {
	f := setup(t, 1)
	f.mon.reprobeGap = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ws := f.st.Workspace(1)
	count := func() int {
		cs, _ := ws.RecentChecks(ctx, f.target.ID, 100)
		return len(cs)
	}
	waitFor(t, "first probe", func() bool { return count() == 1 })
	cur, _ := f.st.TargetByID(ctx, f.target.ID)
	cur.Timeout = 3 * time.Second
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probe after timeout edit", func() bool { return count() == 2 })
	cur.ExpectStatus = 200
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, f.target.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probe after expect_status edit", func() bool { return count() == 3 })
}

// Against a target slower than an edit round trip, a probe an edit triggers is
// cancelled by the next Reload and never recorded, so Last does not advance.
// The floor on the last probe start must still hold runners back: back-to-back
// edits, pause/resume and mute toggles reach the server at most once per gap.
func TestSlowTargetRapidReloadsProbeOncePerGap(t *testing.T) {
	f := setup(t, 1)
	const gap = 500 * time.Millisecond
	f.mon.reprobeGap = gap
	f.mon.now = time.Now // the gap is measured on the monitor's clock, which must move
	var reqs atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		select {
		case <-time.After(300 * time.Millisecond):
			w.Write([]byte("hello"))
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	f.mon.client, f.mon.openClient = slow.Client(), slow.Client()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := f.st.Workspace(1)
	cur, _ := f.st.TargetByID(ctx, f.target.ID)
	cur.URL, cur.Keyword = slow.URL, ""
	if _, err := ws.UpdateTarget(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first request", func() bool { return reqs.Load() >= 1 })
	start := time.Now()
	reloads := 0
	for i := 0; i < 10; i++ { // edits, 100ms apart: ~1s
		cur.Keyword = [2]string{"hello", ""}[i%2]
		if _, err := ws.UpdateTarget(ctx, cur); err != nil {
			t.Fatal(err)
		}
		if err := f.mon.Reload(ctx, f.target.ID); err != nil {
			t.Fatal(err)
		}
		reloads++
		time.Sleep(100 * time.Millisecond)
	}
	for i := 0; i < 10; i++ { // mute toggles only (pause/resume shares the same Reload path), ~1s
		if err := ws.SetMuted(ctx, f.target.ID, i%2 == 0); err != nil {
			t.Fatal(err)
		}
		if err := f.mon.Reload(ctx, f.target.ID); err != nil {
			t.Fatal(err)
		}
		reloads++
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)
	// The first request is counted already; later ones start at least gap apart.
	got := int(reqs.Load()) - 1
	max := int(elapsed/gap) + 1
	if got > max {
		t.Fatalf("%d requests in %v over %d reloads; want at most %d (one per %v)", got, elapsed, reloads, max, gap)
	}
	if got < 1 {
		t.Fatalf("no probe followed the edits")
	}
}

// countingServer answers every probe with 200 and counts them.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// userWorkspace creates a non-operator account with one target probing url.
func userWorkspace(t *testing.T, st *store.Store, url string, now time.Time) (store.User, int64, store.Target) {
	t.Helper()
	ctx := context.Background()
	u, ws, err := st.CreateAccount(ctx, store.AccountInput{Email: "bob@example.com", PasswordHash: "x",
		WorkspaceName: "Bob", Slug: "bob", Verified: true})
	if err != nil {
		t.Fatal(err)
	}
	tg, err := ws.CreateTarget(ctx, store.Target{Name: "bob", URL: url, Interval: time.Hour, Timeout: 2 * time.Second}, now)
	if err != nil {
		t.Fatal(err)
	}
	return u, ws.ID(), tg
}

// A Reload that read the target before a suspension must not leave a runner.
func TestReloadDuringSuspendDoesNotLeaveRunner(t *testing.T) {
	f := setup(t, 1)
	srv, hits := countingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	u, ws, tg := userWorkspace(t, f.st, srv.URL, f.now())
	forgotten := make(chan struct{})
	f.mon.afterReloadCheck = func() {
		if _, err := f.st.SetUserDisabled(ctx, u.ID, true, f.now()); err != nil {
			t.Errorf("SetUserDisabled: %v", err)
		}
		go func() { f.mon.ForgetWorkspace(ws); close(forgotten) }()
		waitFor(t, "tombstone", func() bool {
			f.mon.mu.Lock()
			defer f.mon.mu.Unlock()
			return f.mon.gone[ws]
		})
	}
	if err := f.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	f.mon.afterReloadCheck = nil
	<-forgotten
	f.mon.mu.Lock()
	_, running := f.mon.cancels[tg.ID]
	f.mon.mu.Unlock()
	if running {
		t.Fatal("suspended target has a runner")
	}
	cancel()
	f.mon.Wait()
	if h := hits.Load(); h != 0 {
		t.Fatalf("suspended workspace was probed %d times", h)
	}
}

// RestoreWorkspace lifts the tombstone so a re-enabled user's target runs again.
func TestRestoreWorkspace(t *testing.T) {
	f := setup(t, 1)
	srv, hits := countingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := f.mon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	u, ws, tg := userWorkspace(t, f.st, srv.URL, f.now())
	if _, err := f.st.SetUserDisabled(ctx, u.ID, true, f.now()); err != nil {
		t.Fatal(err)
	}
	f.mon.ForgetWorkspace(ws)
	if err := f.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.mon.Get(ws, tg.ID); ok {
		t.Fatal("suspended target has state")
	}
	if _, err := f.st.SetUserDisabled(ctx, u.ID, false, f.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.mon.Get(ws, tg.ID); ok {
		t.Fatal("tombstoned workspace started a runner after re-enable without RestoreWorkspace")
	}
	f.mon.RestoreWorkspace(ws)
	if err := f.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "probe after restore", func() bool { return hits.Load() > 0 })
}

// An incident open across a suspension stays open: the recovery alert's
// DownFor covers the suspended time.
func TestIncidentSurvivesSuspension(t *testing.T) {
	f := setup(t, 1)
	ctx := context.Background()
	atomic.StoreInt32(&f.status, 503)
	u, ws, tg := userWorkspace(t, f.st, f.srv.URL, f.now())
	if err := f.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(30 * time.Second)
	f.mon.CheckNow(ctx, tg.ID)
	f.mon.WaitAlerts()
	if s, _ := f.mon.Get(ws, tg.ID); s.Status != Down {
		t.Fatalf("want down: %+v", s)
	}
	if _, err := f.st.SetUserDisabled(ctx, u.ID, true, f.now()); err != nil {
		t.Fatal(err)
	}
	f.mon.ForgetWorkspace(ws)
	f.advance(2 * time.Hour)
	if _, err := f.st.SetUserDisabled(ctx, u.ID, false, f.now()); err != nil {
		t.Fatal(err)
	}
	f.mon.RestoreWorkspace(ws)
	if err := f.mon.Reload(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.mon.Get(ws, tg.ID); s.Status != Down || s.IncidentID == 0 {
		t.Fatalf("incident lost across suspension: %+v", s)
	}
	atomic.StoreInt32(&f.status, 200)
	f.advance(30 * time.Second)
	f.mon.CheckNow(ctx, tg.ID)
	f.mon.WaitAlerts()
	evs := f.rec.list()
	if len(evs) != 2 || !evs[1].Up || evs[1].DownFor != 2*time.Hour+30*time.Second {
		t.Fatalf("want up alert with DownFor=2h30s: %+v", evs)
	}
}

// A restarted monitor probes nothing for a disabled user but still probes the operator's target.
func TestDisabledStaysStoppedAfterRestart(t *testing.T) {
	f := setup(t, 1)
	srv, hits := countingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	u, ws, _ := userWorkspace(t, f.st, srv.URL, f.now())
	if _, err := f.st.SetUserDisabled(ctx, u.ID, true, f.now()); err != nil {
		t.Fatal(err)
	}
	m2 := New(Options{Store: f.st, Client: f.srv.Client(), Now: f.now, FailureThreshold: 1})
	if err := m2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "operator probe", func() bool {
		s, ok := m2.Get(1, f.target.ID)
		return ok && s.Last != nil
	})
	cancel()
	m2.Wait()
	if h := hits.Load(); h != 0 {
		t.Fatalf("disabled user's target probed %d times", h)
	}
	if got := m2.Snapshot(ws); len(got) != 0 {
		t.Fatalf("disabled workspace has %d tracked targets", len(got))
	}
}

// flakySender fails its first fail sends and records every attempt.
type flakySender struct {
	mu    sync.Mutex
	fail  int
	calls [][]string
}

func (s *flakySender) Send(_ context.Context, to []string, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, to)
	if s.fail > 0 {
		s.fail--
		return errors.New("smtp down")
	}
	return nil
}

func (s *flakySender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// serverEmailFixture routes the fixture's alerts through the fallback email.
func serverEmailFixture(t *testing.T, fail int) (*fixture, *mail.Queue, *flakySender) {
	t.Helper()
	f := setup(t, 1)
	f.mon.states[f.target.ID] = &state{State: State{Target: f.target}}
	snd := &flakySender{fail: fail}
	q := mail.New(mail.Options{Sender: snd, PerHour: 100, Now: f.now})
	f.mon.SetNotifiers(1, notify.Multi{{Name: notify.ServerEmailName, Notifier: notify.ServerEmail{Workspace: 1, Mail: q,
		Recipients: func(context.Context) ([]string, error) { return []string{"owner@example.com"}, nil }}}})
	return f, q, snd
}

func TestServerEmailChargedOncePerAlertAcrossRetries(t *testing.T) {
	f, q, snd := serverEmailFixture(t, 2) // two failed attempts, then delivered
	atomic.StoreInt32(&f.status, 500)
	f.tick(t)
	if snd.count() != 3 {
		t.Fatalf("attempts = %d, want 3", snd.count())
	}
	last, _ := f.st.Workspace(1).LastAlerts(context.Background())
	if a := last[notify.ServerEmailName]; !a.OK {
		t.Fatalf("alert not recorded as delivered: %+v", a)
	}
	// One alert used one of the 10 hourly slots, not three.
	for i := 0; i < 9; i++ {
		if r := q.Admit(1, 1); r != "" {
			t.Fatalf("admit %d refused (%s): the retries were charged", i+2, r)
		}
	}
	if r := q.Admit(1, 1); r != "workspace_hour" {
		t.Fatalf("10 alerts should spend the hour budget, got %q", r)
	}
}

func TestServerEmailOverBudgetRecordsLimitReached(t *testing.T) {
	f, q, snd := serverEmailFixture(t, 0)
	for i := 0; i < 10; i++ {
		q.Admit(1, 1)
	}
	atomic.StoreInt32(&f.status, 500)
	f.tick(t)
	if snd.count() != 0 {
		t.Fatalf("a dropped alert was sent %d times", snd.count())
	}
	last, _ := f.st.Workspace(1).LastAlerts(context.Background())
	if a := last[notify.ServerEmailName]; a.OK || a.Error != "Limit reached" {
		t.Fatalf("status = %+v, want a failed alert reading Limit reached", a)
	}
}
