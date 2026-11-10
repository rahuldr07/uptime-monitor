// Package monitor schedules checks, tracks per-target state and raises alerts.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"uptimemon/internal/netguard"
	"uptimemon/internal/notify"
	"uptimemon/internal/store"
)

// Status is the health of a target as the monitor sees it.
type Status int

const (
	Unknown Status = iota
	Up
	Down
	Paused
)

func (s Status) String() string {
	switch s {
	case Up:
		return "up"
	case Down:
		return "down"
	case Paused:
		return "paused"
	default:
		return "unknown"
	}
}

// State is a snapshot of one target.
type State struct {
	Target     store.Target
	Status     Status
	Since      time.Time // when the current status began
	Last       *store.Check
	Failures   int // consecutive failures so far
	IncidentID int64
}

type state struct {
	State
	firstFailAt time.Time
	checkMu     sync.Mutex // serialises probe+apply per target
	busy        bool       // a check is running or queued; guarded by Monitor.mu
}

// Options configure a Monitor.
type Options struct {
	Store *store.Store
	// Notifiers loads one workspace's alert channels. It is called on first
	// use of a workspace and again after a failure; it must not be called
	// while the monitor lock is held (the monitor never does).
	Notifiers        func(ctx context.Context, workspaceID int64) (notify.Multi, error)
	Logger           *slog.Logger
	FailureThreshold int
	Client           *http.Client // tests: used for every workspace
	// PrivateAllowed reports whether a workspace may probe internal
	// addresses. Nil guards every workspace (fails closed).
	PrivateAllowed func(workspaceID int64) bool
	Now            func() time.Time
	RetryBase      time.Duration // backoff base for alert retries
	UserAgent      string        // sent on every probe; empty means "uptime-monitor/1.0"
}

// Event is published whenever a target's state or latest check changes.
type Event struct {
	TargetID    int64  `json:"target_id"`
	WorkspaceID int64  `json:"-"`
	Kind        string `json:"kind"` // "check", "down", "up", "reload", "removed", "maintenance"
}

// Bus fans Events out to subscribers without blocking the publisher.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// Subscribe returns a channel of events and a function to unsubscribe.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = map[chan Event]struct{}{}
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Publish broadcasts an event. Other packages use it for changes the
// monitor does not see itself, such as a maintenance window being scheduled.
func (b *Bus) Publish(ev Event) { b.publish(ev) }

func (b *Bus) publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow subscriber: drop, it will catch up on the next event
		}
	}
}

// Monitor runs one goroutine per target.
type Monitor struct {
	Events Bus

	store      *store.Store
	logger     *slog.Logger
	client     *http.Client // guarded: refuses internal addresses
	openClient *http.Client // unguarded, for workspaces where PrivateAllowed is true
	private    func(int64) bool
	loadNotif  func(context.Context, int64) (notify.Multi, error)
	threshold  int
	now        func() time.Time
	retryBase  time.Duration
	userAgent  string

	notifMu   sync.Mutex // guards notifiers; never held with mu or around loadNotif
	notifiers map[int64]notify.Multi

	mu      sync.Mutex
	states  map[int64]*state
	cancels map[int64]context.CancelFunc
	ctx     context.Context
	wg      sync.WaitGroup
	alertWG sync.WaitGroup
}

// New creates a Monitor. Call Start to begin checking.
func New(o Options) *Monitor {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	guarded, open := o.Client, o.Client
	if o.Client == nil {
		guarded, open = probeClient(false), probeClient(true)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.FailureThreshold <= 0 {
		o.FailureThreshold = 1
	}
	if o.RetryBase <= 0 {
		o.RetryBase = time.Second
	}
	return &Monitor{
		store:      o.Store,
		logger:     o.Logger,
		client:     guarded,
		openClient: open,
		private:    o.PrivateAllowed,
		loadNotif:  o.Notifiers,
		threshold:  o.FailureThreshold,
		now:        o.Now,
		retryBase:  o.RetryBase,
		userAgent:  o.UserAgent,
		notifiers:  map[int64]notify.Multi{},
		states:     map[int64]*state{},
		cancels:    map[int64]context.CancelFunc{},
	}
}

// probeClient builds a netguard client and keeps probes on HTTP/1.1.
func probeClient(allowPrivate bool) *http.Client {
	c := netguard.Client(allowPrivate, 0)
	tr := c.Transport.(*http.Transport)
	tr.TLSHandshakeTimeout = 10 * time.Second
	tr.MaxIdleConnsPerHost = 2
	tr.IdleConnTimeout = 90 * time.Second
	tr.DisableKeepAlives = true
	// Probes stay on HTTP/1.1: ForceAttemptHTTP2=false alone still leaves
	// "h2" in the cloned TLS config's ALPN, so an h2 server would be
	// negotiated and then spoken to in HTTP/1.1 (EOF). Clear both.
	tr.ForceAttemptHTTP2 = false
	if tr.TLSClientConfig != nil {
		tr.TLSClientConfig.NextProtos = nil
	}
	return c
}

// clientFor returns the probe client for a workspace.
func (m *Monitor) clientFor(ws int64) *http.Client {
	if m.private != nil && m.private(ws) {
		return m.openClient
	}
	return m.client
}

// Start loads targets from the store, restores their state and starts a
// runner for each. It returns immediately; use Wait to block.
func (m *Monitor) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	targets, err := m.store.AllTargets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := m.Reload(ctx, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// Wait blocks until every runner and pending alert has finished.
func (m *Monitor) Wait() {
	m.wg.Wait()
	m.alertWG.Wait()
}

// WaitAlerts blocks until pending alert deliveries finish. Used by tests.
func (m *Monitor) WaitAlerts() { m.alertWG.Wait() }

// SetNotifiers replaces one workspace's alert channels at runtime.
func (m *Monitor) SetNotifiers(ws int64, n notify.Multi) {
	m.notifMu.Lock()
	defer m.notifMu.Unlock()
	m.notifiers[ws] = n
}

// Notifiers returns a workspace's alert channels, loading them on first use.
// If loading fails the error is logged and the log channel is returned for
// this call only, so the next call tries again.
func (m *Monitor) Notifiers(ctx context.Context, ws int64) notify.Multi {
	m.notifMu.Lock()
	n, ok := m.notifiers[ws]
	m.notifMu.Unlock()
	if ok {
		return n
	}
	fallback := notify.Multi{{Name: "log", Notifier: notify.Log{Logger: m.logger}}}
	if m.loadNotif == nil {
		return fallback
	}
	n, err := m.loadNotif(ctx, ws)
	if err != nil {
		m.logger.Error("load notifiers", "workspace", ws, "err", err)
		return fallback
	}
	m.notifMu.Lock()
	defer m.notifMu.Unlock()
	if cur, ok := m.notifiers[ws]; ok {
		return cur // SetNotifiers won the race
	}
	m.notifiers[ws] = n
	return n
}

// Reload (re)starts the runner for a target after it was created or edited.
// It re-reads the target from the store; a target that no longer exists is
// removed. State such as an open incident is restored from the store.
func (m *Monitor) Reload(ctx context.Context, targetID int64) error {
	t, err := m.store.TargetByID(ctx, targetID)
	if errors.Is(err, store.ErrNotFound) {
		m.Remove(targetID)
		return nil
	}
	if err != nil {
		return err
	}
	m.mu.Lock()
	if cancel, ok := m.cancels[t.ID]; ok {
		cancel()
		delete(m.cancels, t.ID)
	}
	st, ok := m.states[t.ID]
	if !ok {
		st = &state{}
		m.states[t.ID] = st
	}
	runCtx := m.ctx
	m.mu.Unlock()
	// Wait for any probe+apply in flight so a state transition cannot land
	// in the middle of an incident being opened or closed.
	st.checkMu.Lock()
	defer st.checkMu.Unlock()
	m.mu.Lock()
	st.Target = t
	m.mu.Unlock()

	if !ok {
		if err := m.restore(ctx, st); err != nil {
			return err
		}
	}
	if t.Paused {
		m.mu.Lock()
		var closeID int64
		if st.Status != Paused {
			closeID = st.IncidentID
			st.Status = Paused
			st.Since = m.now()
			st.IncidentID = 0
			st.Failures = 0
			st.firstFailAt = time.Time{}
		}
		m.mu.Unlock()
		if closeID != 0 {
			// Pausing ends the incident; monitoring stops, so it cannot be "ongoing".
			if err := m.store.CloseIncident(ctx, closeID, m.now().UTC()); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		m.Events.publish(Event{TargetID: t.ID, WorkspaceID: t.WorkspaceID, Kind: "reload"})
		return nil
	}
	m.mu.Lock()
	if st.Status == Paused {
		st.Status = Unknown
		st.Since = m.now()
		st.Failures = 0
		st.IncidentID = 0
		st.firstFailAt = time.Time{}
	}
	m.mu.Unlock()
	if runCtx == nil {
		return nil // Start not called yet
	}
	rctx, cancel := context.WithCancel(runCtx)
	m.mu.Lock()
	m.cancels[t.ID] = cancel
	m.mu.Unlock()
	m.wg.Add(1)
	go m.run(rctx, t.ID)
	m.Events.publish(Event{TargetID: t.ID, WorkspaceID: t.WorkspaceID, Kind: "reload"})
	return nil
}

// Remove stops and forgets a target.
func (m *Monitor) Remove(id int64) {
	m.mu.Lock()
	if cancel, ok := m.cancels[id]; ok {
		cancel()
		delete(m.cancels, id)
	}
	st := m.states[id]
	var ws int64
	if st != nil {
		ws = st.Target.WorkspaceID
	}
	m.mu.Unlock()
	if st != nil {
		st.checkMu.Lock() // wait for an in-flight probe so it cannot write after the delete
		defer st.checkMu.Unlock()
	}
	m.mu.Lock()
	delete(m.states, id)
	m.mu.Unlock()
	m.Events.publish(Event{TargetID: id, WorkspaceID: ws, Kind: "removed"})
}

// restore sets initial state from stored history so a restart does not
// re-alert or forget an ongoing incident.
func (m *Monitor) restore(ctx context.Context, st *state) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := st.Target.ID
	if in, err := m.store.OngoingIncident(ctx, id); err == nil {
		st.Status = Down
		st.Since = in.StartedAt
		st.IncidentID = in.ID
		st.Failures = m.threshold
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	c, err := m.store.LatestCheck(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	st.Last = &c
	if st.Status == Unknown && c.OK {
		st.Status = Up
		st.Since = c.CheckedAt
	}
	return nil
}

// Snapshot returns the current state of every target in a workspace, sorted by name.
func (m *Monitor) Snapshot(ws int64) []State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.states))
	for _, st := range m.states {
		if st.Target.WorkspaceID != ws {
			continue
		}
		out = append(out, st.State)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Target.Name) < strings.ToLower(out[j].Target.Name)
	})
	return out
}

// Get returns the state of one target in a workspace.
func (m *Monitor) Get(ws, id int64) (State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[id]
	if !ok || st.Target.WorkspaceID != ws {
		return State{}, false
	}
	return st.State, true
}

func (m *Monitor) run(ctx context.Context, id int64) {
	defer m.wg.Done()
	m.mu.Lock()
	st := m.states[id]
	var interval time.Duration
	if st != nil {
		interval = st.Target.Interval
	}
	m.mu.Unlock()
	if st == nil {
		return
	}
	// A reload (edit, pause, mute) restarts this loop; do not probe again
	// when the last check is still fresh.
	m.mu.Lock()
	fresh := st.Last != nil && m.now().Sub(st.Last.CheckedAt) < interval
	m.mu.Unlock()
	if !fresh {
		m.CheckNow(ctx, id)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.CheckNow(ctx, id)
		}
	}
}

// claim marks the target busy. It returns nil when the target is gone or a
// check is already running or queued.
func (m *Monitor) claim(id int64) *state {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[id]
	if !ok || st.busy {
		return nil
	}
	st.busy = true
	return st
}

func (m *Monitor) release(st *state) {
	m.mu.Lock()
	st.busy = false
	m.mu.Unlock()
}

// CheckNow probes a target once and applies the result. It is safe to call
// from the web UI while the runner is active. It does nothing when another
// check of the target is already running or queued.
func (m *Monitor) CheckNow(ctx context.Context, id int64) {
	st := m.claim(id)
	if st == nil {
		return
	}
	defer m.release(st)
	m.checkClaimed(ctx, id, st)
}

func (m *Monitor) checkClaimed(ctx context.Context, id int64, st *state) {
	m.mu.Lock()
	t := st.Target
	m.mu.Unlock()
	// One probe+apply at a time per target, so a manual "check now" cannot
	// interleave with the runner between the state change and the store write.
	st.checkMu.Lock()
	defer st.checkMu.Unlock()
	m.mu.Lock()
	cur, still := m.states[id]
	if still {
		t = cur.Target
	}
	m.mu.Unlock()
	if !still || cur != st || t.Paused {
		return // removed, replaced or paused while we waited
	}
	c := probe(ctx, m.clientFor(t.WorkspaceID), t, m.now, m.userAgent)
	if ctx.Err() != nil {
		return // shutting down; do not record a cancelled probe
	}
	// The result is accepted: persist it even if the caller's context is
	// cancelled from here on, so state and store never disagree.
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, in, err := m.store.Workspace(t.WorkspaceID).InMaintenance(sctx, t.Group, c.CheckedAt); err != nil {
		m.logger.Error("maintenance lookup", "target", t.Name, "err", err)
	} else {
		c.Maintenance = in
	}
	if err := m.store.InsertCheck(sctx, c); err != nil {
		m.logger.Error("store check", "target", t.Name, "err", err)
	}
	m.apply(sctx, id, c)
}

// CheckAsync runs CheckNow in the background, detached from the caller. It
// reports false, and starts nothing, when the target is not in workspace ws or
// a check of it (manual or scheduled) is still running or queued.
func (m *Monitor) CheckAsync(ws, id int64) bool {
	m.mu.Lock()
	ctx := m.ctx
	st, ok := m.states[id]
	if !ok || st.Target.WorkspaceID != ws || st.busy {
		m.mu.Unlock()
		return false
	}
	st.busy = true
	m.wg.Add(1)
	m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		defer m.wg.Done()
		defer m.release(st)
		m.checkClaimed(ctx, id, st)
	}()
	return true
}

// Test probes an unsaved target with the workspace's probe client.
func (m *Monitor) Test(ctx context.Context, ws int64, t store.Target) store.Check {
	return probe(ctx, m.clientFor(ws), t, m.now, m.userAgent)
}

// Probe performs one HTTP check of a target.
func Probe(ctx context.Context, client *http.Client, t store.Target, now func() time.Time) store.Check {
	return probe(ctx, client, t, now, "")
}

func probe(ctx context.Context, client *http.Client, t store.Target, now func() time.Time, userAgent string) store.Check {
	if userAgent == "" {
		userAgent = "uptime-monitor/1.0"
	}
	c := store.Check{TargetID: t.ID, CheckedAt: now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	req.Header.Set("User-Agent", userAgent)
	start := time.Now()
	resp, err := client.Do(req)
	c.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		c.Error = describeErr(err, t.Timeout)
		return c
	}
	defer resp.Body.Close()
	c.StatusCode = resp.StatusCode
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	c.LatencyMS = time.Since(start).Milliseconds()
	switch {
	case t.ExpectStatus != 0 && resp.StatusCode != t.ExpectStatus:
		c.Error = fmt.Sprintf("expected HTTP %d, got %d", t.ExpectStatus, resp.StatusCode)
	case t.ExpectStatus == 0 && (resp.StatusCode < 200 || resp.StatusCode >= 400):
		c.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	case readErr != nil:
		c.Error = "read body: " + describeErr(readErr, t.Timeout)
	case t.Keyword != "" && !strings.Contains(string(body), t.Keyword):
		c.Error = fmt.Sprintf("keyword %q not found in body", t.Keyword)
	default:
		c.OK = true
	}
	return c
}

func describeErr(err error, timeout time.Duration) string {
	if errors.Is(err, netguard.ErrBlocked) {
		return netguard.ErrBlocked.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("timeout after %s", timeout)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Sprintf("timeout after %s", timeout)
	}
	s := err.Error()
	// Strip the "Get \"url\": " prefix url.Error adds; the URL is shown elsewhere.
	if i := strings.Index(s, "\": "); i >= 0 && strings.HasPrefix(s, "Get \"") {
		s = s[i+3:]
	}
	return s
}

// apply feeds a check result through the state machine.
func (m *Monitor) apply(ctx context.Context, id int64, c store.Check) {
	m.mu.Lock()
	st, ok := m.states[id]
	if !ok || st.Target.Paused {
		m.mu.Unlock()
		return
	}
	cc := c
	st.Last = &cc
	t := st.Target
	var ev *notify.Event
	var incidentID int64
	var downSince time.Time

	if c.OK {
		st.Failures = 0
		st.firstFailAt = time.Time{}
		prev := st.Status
		if prev != Up {
			st.Status = Up
			since := st.Since
			st.Since = c.CheckedAt
			if prev == Down {
				incidentID = st.IncidentID
				st.IncidentID = 0
				ev = &notify.Event{Target: t.Name, URL: t.URL, Up: true, At: c.CheckedAt,
					StatusCode: c.StatusCode, DownFor: c.CheckedAt.Sub(since)}
			}
		}
	} else if c.Maintenance {
		// Expected downtime: show the failure but do not count it toward an
		// incident. Failures only start counting once the window is over.
		st.Failures = 0
		st.firstFailAt = time.Time{}
	} else {
		st.Failures++
		if st.firstFailAt.IsZero() {
			st.firstFailAt = c.CheckedAt
		}
		if st.Status != Down && st.Failures >= m.threshold {
			st.Status = Down
			st.Since = st.firstFailAt
			downSince = st.firstFailAt
			ev = &notify.Event{Target: t.Name, URL: t.URL, Up: false, At: c.CheckedAt,
				StatusCode: c.StatusCode, Error: c.Error}
		}
	}
	m.mu.Unlock()

	if ev == nil {
		m.Events.publish(Event{TargetID: id, WorkspaceID: t.WorkspaceID, Kind: "check"})
		return
	}
	if ev.Up {
		if err := m.store.CloseIncident(ctx, incidentID, c.CheckedAt); err != nil {
			m.logger.Error("close incident", "target", t.Name, "err", err)
		}
		m.logger.Info("target recovered", "target", t.Name, "down_for", ev.DownFor)
	} else {
		cause := c.Error
		var err error
		incidentID, err = m.store.OpenIncident(ctx, t.ID, downSince, cause)
		if err != nil {
			m.logger.Error("open incident", "target", t.Name, "err", err)
		}
		m.mu.Lock()
		st.IncidentID = incidentID
		m.mu.Unlock()
		m.logger.Warn("target down", "target", t.Name, "cause", cause)
	}
	kind := "down"
	if ev.Up {
		kind = "up"
	}
	m.Events.publish(Event{TargetID: id, WorkspaceID: t.WorkspaceID, Kind: kind})
	if t.Muted {
		m.logger.Info("alerts muted; not notifying", "target", t.Name)
		return
	}
	m.alertWG.Add(1)
	go m.alert(incidentID, t.ID, t.WorkspaceID, *ev)
}

// alert delivers an event to every channel with retries and records each
// outcome. Channels are delivered concurrently so one slow or failing
// channel does not delay the others. It runs detached from the request
// context so a shutdown does not drop an alert that is already in flight.
func (m *Monitor) alert(incidentID, targetID, ws int64, ev notify.Event) {
	defer m.alertWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	event := "down"
	if ev.Up {
		event = "up"
	}
	var wg sync.WaitGroup
	for _, n := range m.Notifiers(ctx, ws) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := m.deliver(ctx, n, ev)
			a := store.Alert{IncidentID: incidentID, TargetID: targetID, SentAt: m.now().UTC(),
				Channel: n.Name, Event: event, OK: err == nil}
			if err != nil {
				a.Error = err.Error()
				m.logger.Error("alert not delivered", "channel", n.Name, "target", ev.Target, "err", err)
			}
			// Record the outcome even if the delivery used up the whole budget.
			sctx, scancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer scancel()
			if serr := m.store.InsertAlert(sctx, a); serr != nil {
				m.logger.Error("store alert", "err", serr)
			}
		}()
	}
	wg.Wait()
}

// deliver sends ev through one channel, retrying with exponential backoff.
func (m *Monitor) deliver(ctx context.Context, n notify.Named, ev notify.Event) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(m.retryBase << (attempt - 1)):
			case <-ctx.Done():
				return err
			}
		}
		// Each attempt gets its own deadline so one hung attempt leaves time to retry.
		actx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = n.Notify(actx, ev)
		cancel()
		if err == nil || ctx.Err() != nil {
			return err
		}
		m.logger.Warn("alert failed", "channel", n.Name, "attempt", attempt+1, "err", err)
	}
	return err
}

// SendTest sends a synthetic event through one channel by name.
func (m *Monitor) SendTest(ctx context.Context, ws int64, channel string) error {
	for _, n := range m.Notifiers(ctx, ws) {
		if n.Name == channel {
			return n.Notify(ctx, notify.Event{Target: "test", URL: "https://example.com", Up: true, At: m.now()})
		}
	}
	return fmt.Errorf("no channel named %q", channel)
}
