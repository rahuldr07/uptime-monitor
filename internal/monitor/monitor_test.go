package monitor

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"uptimemon/internal/notify"
	"uptimemon/internal/store"
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

type fixture struct {
	st     *store.Store
	mon    *Monitor
	rec    *recorder
	target store.Target
	status int32
	body   string
	clock  time.Time
	srv    *httptest.Server
}

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
	f.clock = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	f.target, _ = st.CreateTarget(context.Background(), store.Target{Name: "api", URL: f.srv.URL, Interval: time.Hour, Timeout: 2 * time.Second, Keyword: "hello"}, f.clock)
	f.rec = &recorder{}
	f.mon = New(Options{Store: st, Notifiers: notify.Multi{{Name: "rec", Notifier: f.rec}}, Logger: slog.Default(),
		FailureThreshold: threshold, Client: f.srv.Client(), Now: func() time.Time { return f.clock }, RetryBase: time.Millisecond})
	return f
}

func (f *fixture) tick(t *testing.T) State {
	t.Helper()
	f.clock = f.clock.Add(30 * time.Second)
	f.mon.CheckNow(context.Background(), f.target.ID)
	f.mon.WaitAlerts()
	s, _ := f.mon.Get(f.target.ID)
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
	if !s.Since.Equal(f.clock.Add(-30 * time.Second)) {
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
	if n, _ := f.st.CountAlerts(ctx, time.Time{}); n != 2 {
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
	id, _ := f.st.OpenIncident(ctx, f.target.ID, f.clock.Add(-time.Hour), "HTTP 500")
	f.st.InsertCheck(ctx, store.Check{TargetID: f.target.ID, CheckedAt: f.clock.Add(-time.Minute), OK: false, StatusCode: 500})
	if err := f.mon.Reload(ctx, f.target); err != nil {
		t.Fatal(err)
	}
	s, _ := f.mon.Get(f.target.ID)
	if s.Status != Down || s.IncidentID != id || !s.Since.Equal(f.clock.Add(-time.Hour)) {
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
		if s, _ := f.mon.Get(f.target.ID); s.Status == Up {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runner never checked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.target.Paused = true
	f.mon.Reload(ctx, f.target)
	if s, _ := f.mon.Get(f.target.ID); s.Status != Paused {
		t.Fatalf("want paused: %+v", s)
	}
	f.target.Paused = false
	f.mon.Reload(ctx, f.target)
	if s, _ := f.mon.Get(f.target.ID); s.Status == Paused {
		t.Fatalf("want resumed: %+v", s)
	}
	f.mon.Remove(f.target.ID)
	if _, ok := f.mon.Get(f.target.ID); ok {
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
	f.target.Paused = true
	if err := f.mon.Reload(ctx, f.target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.OngoingIncident(ctx, f.target.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("pausing should close the incident: %v", err)
	}
	f.target.Paused = false
	f.mon.Reload(ctx, f.target)
	s, _ = f.mon.Get(f.target.ID)
	if s.Status != Unknown || s.IncidentID != 0 || s.Failures != 0 {
		t.Fatalf("resume should reset state: %+v", s)
	}
	// Still failing after resume: a fresh incident starting now, not before the pause.
	before := len(f.rec.list())
	s = f.tick(t)
	if s.Status != Down || !s.Since.Equal(f.clock) {
		t.Fatalf("new incident should start at the post-resume failure: %+v (clock %v)", s, f.clock)
	}
	if list, _ := f.st.ListIncidents(ctx, store.IncidentFilter{OngoingOnly: true}); len(list) != 1 {
		t.Fatalf("exactly one ongoing incident, got %d", len(list))
	}
	if len(f.rec.list()) != before+1 {
		t.Fatal("expected a new down alert after resume")
	}
	atomic.StoreInt32(&f.status, 200)
	if s = f.tick(t); s.Status != Up {
		t.Fatalf("want up: %+v", s)
	}
	if list, _ := f.st.ListIncidents(ctx, store.IncidentFilter{OngoingOnly: true}); len(list) != 0 {
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
			f.mon.CheckNow(context.Background(), f.target.ID)
		}(i)
	}
	wg.Wait()
	f.mon.WaitAlerts()
	list, _ := f.st.ListIncidents(context.Background(), store.IncidentFilter{OngoingOnly: true})
	s, _ := f.mon.Get(f.target.ID)
	if (s.Status == Down) != (len(list) == 1) || len(list) > 1 {
		t.Fatalf("state %v but %d ongoing incidents", s.Status, len(list))
	}
}
