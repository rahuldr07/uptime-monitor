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
}

// Options configure a Monitor.
type Options struct {
	Store            *store.Store
	Notifiers        notify.Multi
	Logger           *slog.Logger
	FailureThreshold int
	Client           *http.Client
	Now              func() time.Time
	RetryBase        time.Duration // backoff base for alert retries
}

// Monitor runs one goroutine per target.
type Monitor struct {
	store     *store.Store
	logger    *slog.Logger
	client    *http.Client
	threshold int
	now       func() time.Time
	retryBase time.Duration

	mu        sync.Mutex
	notifiers notify.Multi
	states    map[int64]*state
	cancels   map[int64]context.CancelFunc
	ctx       context.Context
	wg        sync.WaitGroup
	alertWG   sync.WaitGroup
}

// New creates a Monitor. Call Start to begin checking.
func New(o Options) *Monitor {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Client == nil {
		o.Client = &http.Client{Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
			DisableKeepAlives:   true,
		}}
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
		store:     o.Store,
		logger:    o.Logger,
		client:    o.Client,
		threshold: o.FailureThreshold,
		now:       o.Now,
		retryBase: o.RetryBase,
		notifiers: o.Notifiers,
		states:    map[int64]*state{},
		cancels:   map[int64]context.CancelFunc{},
	}
}

// Start loads targets from the store, restores their state and starts a
// runner for each. It returns immediately; use Wait to block.
func (m *Monitor) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	targets, err := m.store.ListTargets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := m.Reload(ctx, t); err != nil {
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

// SetNotifiers replaces the alert channels at runtime.
func (m *Monitor) SetNotifiers(n notify.Multi) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notifiers = n
}

// Notifiers returns the current alert channels.
func (m *Monitor) Notifiers() notify.Multi {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notifiers
}

// Reload (re)starts the runner for a target after it was created or edited.
// State such as an open incident is restored from the store.
func (m *Monitor) Reload(ctx context.Context, t store.Target) error {
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
	st.Target = t
	runCtx := m.ctx
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
	return nil
}

// Remove stops and forgets a target.
func (m *Monitor) Remove(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.cancels[id]; ok {
		cancel()
		delete(m.cancels, id)
	}
	delete(m.states, id)
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

// Snapshot returns the current state of every target, sorted by name.
func (m *Monitor) Snapshot() []State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]State, 0, len(m.states))
	for _, st := range m.states {
		out = append(out, st.State)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Target.Name) < strings.ToLower(out[j].Target.Name)
	})
	return out
}

// Get returns the state of one target.
func (m *Monitor) Get(id int64) (State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.states[id]
	if !ok {
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
	m.CheckNow(ctx, id)
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

// CheckNow probes a target once and applies the result. It is safe to call
// from the web UI while the runner is active.
func (m *Monitor) CheckNow(ctx context.Context, id int64) {
	m.mu.Lock()
	st, ok := m.states[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	t := st.Target
	m.mu.Unlock()
	// One probe+apply at a time per target, so a manual "check now" cannot
	// interleave with the runner between the state change and the store write.
	st.checkMu.Lock()
	defer st.checkMu.Unlock()
	c := Probe(ctx, m.client, t, m.now)
	if ctx.Err() != nil {
		return // shutting down; do not record a cancelled probe
	}
	// The result is accepted: persist it even if the caller's context is
	// cancelled from here on, so state and store never disagree.
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := m.store.InsertCheck(sctx, c); err != nil {
		m.logger.Error("store check", "target", t.Name, "err", err)
	}
	m.apply(sctx, id, c)
}

// CheckAsync runs CheckNow in the background, detached from the caller.
func (m *Monitor) CheckAsync(id int64) {
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.CheckNow(ctx, id)
	}()
}

// Probe performs one HTTP check of a target.
func Probe(ctx context.Context, client *http.Client, t store.Target, now func() time.Time) store.Check {
	c := store.Check{TargetID: t.ID, CheckedAt: now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	req.Header.Set("User-Agent", "uptime-monitor/1.0")
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
	m.alertWG.Add(1)
	go m.alert(incidentID, t.ID, *ev)
}

// alert delivers an event to every channel with retries and records each
// outcome. It runs detached from the request context so a shutdown does not
// drop an alert that is already in flight.
func (m *Monitor) alert(incidentID, targetID int64, ev notify.Event) {
	defer m.alertWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	event := "down"
	if ev.Up {
		event = "up"
	}
	for _, n := range m.Notifiers() {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(m.retryBase << (attempt - 1)):
				case <-ctx.Done():
				}
			}
			err = n.Notify(ctx, ev)
			if err == nil || ctx.Err() != nil {
				break
			}
			m.logger.Warn("alert failed", "channel", n.Name, "attempt", attempt+1, "err", err)
		}
		a := store.Alert{IncidentID: incidentID, TargetID: targetID, SentAt: m.now().UTC(),
			Channel: n.Name, Event: event, OK: err == nil}
		if err != nil {
			a.Error = err.Error()
		}
		if serr := m.store.InsertAlert(ctx, a); serr != nil {
			m.logger.Error("store alert", "err", serr)
		}
	}
}

// SendTest sends a synthetic event through one channel by name.
func (m *Monitor) SendTest(ctx context.Context, channel string) error {
	for _, n := range m.Notifiers() {
		if n.Name == channel {
			return n.Notify(ctx, notify.Event{Target: "test", URL: "https://example.com", Up: true, At: m.now()})
		}
	}
	return fmt.Errorf("no channel named %q", channel)
}
