package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/config"
	"github.com/rahuldr07/uptime-monitor/internal/monitor"
	"github.com/rahuldr07/uptime-monitor/internal/netguard"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// ---- overview & monitors ----

type overviewData struct {
	base
	Rows        []targetView
	Total       int
	Up, Down    int
	PausedN     int
	Pending     int
	LongestDown *targetView
	Overall24   store.Stats
	P95         int64
	Incidents7  int
	Alerts7     int
	HasChannel  bool
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	ctx := r.Context()
	d := overviewData{base: s.base(r, a, "overview", "Overview")}
	views, err := s.views(r.Context(), ws, viewOpts{spark: true})
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Rows = views
	d.Total = len(views)
	for i := range views {
		switch views[i].State.Status {
		case monitor.Down:
			d.Down++
			if d.LongestDown == nil || views[i].DownFor > d.LongestDown.DownFor {
				d.LongestDown = &views[i]
			}
		case monitor.Up:
			d.Up++
		case monitor.Paused:
			d.PausedN++
		default:
			d.Pending++
		}
	}
	now := s.now()
	if d.Overall24, err = ws.Stats(ctx, 0, now.Add(-24*time.Hour)); err != nil {
		s.fail(w, err)
		return
	}
	lat, err := ws.Latencies(ctx, 0, now.Add(-24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	d.P95 = store.Percentile(lat, 95)
	if d.Incidents7, err = ws.CountIncidents(ctx, now.Add(-7*24*time.Hour)); err != nil {
		s.fail(w, err)
		return
	}
	if d.Alerts7, err = ws.CountAlerts(ctx, now.Add(-7*24*time.Hour)); err != nil {
		s.fail(w, err)
		return
	}
	for _, n := range s.mon.Notifiers(r.Context(), ws.ID()) {
		if n.Name != "log" {
			d.HasChannel = true
		}
	}
	s.render(w, r, "overview", d)
}

type monitorsData struct {
	base
	Rows []targetView
}

func (s *Server) monitorsPage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	d := monitorsData{base: s.base(r, a, "monitors", "Monitors")}
	var err error
	if d.Rows, err = s.views(r.Context(), ws, viewOpts{}); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "monitors", d)
}

// ---- target form ----

type targetForm struct {
	Action       string
	Submit       string
	ShowTest     bool
	ShowFlags    bool
	Name         string
	URL          string
	Interval     string
	Timeout      string
	ExpectStatus string
	Keyword      string
	Group        string
	Muted        bool
}

func formFromTarget(t store.Target) targetForm {
	f := targetForm{Action: fmt.Sprintf("/targets/%d", t.ID), Submit: "Save changes", ShowFlags: true, Name: t.Name, URL: t.URL,
		Interval: shortDur(t.Interval), Timeout: shortDur(t.Timeout), Keyword: t.Keyword, Group: t.Group, Muted: t.Muted}
	if t.ExpectStatus != 0 {
		f.ExpectStatus = strconv.Itoa(t.ExpectStatus)
	}
	return f
}

func formFromRequest(r *http.Request, action, submit string) targetForm {
	return targetForm{Action: action, Submit: submit,
		Name: strings.TrimSpace(r.FormValue("name")), URL: strings.TrimSpace(r.FormValue("url")),
		Interval: strings.TrimSpace(r.FormValue("interval")), Timeout: strings.TrimSpace(r.FormValue("timeout")),
		ExpectStatus: strings.TrimSpace(r.FormValue("expect_status")), Keyword: r.FormValue("keyword"),
		Group: strings.TrimSpace(r.FormValue("group")), Muted: r.FormValue("muted") == "1"}
}

// retryAfter sets the Retry-After header from a limiter's wait.
func retryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(d.Seconds())))))
}

func wsKey(a *authCtx) string { return strconv.FormatInt(a.WS.ID(), 10) }

// hostEpsilon absorbs float rounding, so a total of exactly the limit passes.
const hostEpsilon = 1e-9

// limitErr applies the per-workspace limits that the operator is exempt from:
// minimum interval and the combined per-host check rate. (The monitor cap is
// enforced inside the create transaction.) selfID is the target being edited, or 0 for a new one.
func (s *Server) limitErr(ctx context.Context, a *authCtx, t store.Target, selfID int64) error {
	if a.IsOperator {
		return nil
	}
	lim := s.cfg.Limits
	if t.Interval < lim.MinInterval.Duration {
		return fmt.Errorf("Check interval must be at least %s.", shortDur(lim.MinInterval.Duration))
	}
	return s.hostRateErr(ctx, a, t, selfID)
}

// hostRateErr refuses a target that would push the combined check rate to its
// host, across every non-operator workspace, over limits.host_requests_per_second.
func (s *Server) hostRateErr(ctx context.Context, a *authCtx, t store.Target, selfID int64) error {
	host := hostOf(t.URL)
	rate := 1 / t.Interval.Seconds()
	others, err := s.sys.TargetsInOtherWorkspaces(ctx, a.WS.ID())
	if err != nil {
		return err
	}
	own, err := a.WS.ListTargets(ctx)
	if err != nil {
		return err
	}
	for _, o := range append(others, own...) {
		if o.ID != selfID && hostOf(o.URL) == host && o.Interval > 0 {
			rate += 1 / o.Interval.Seconds()
		}
	}
	if rate > *s.cfg.Limits.HostRequestsPerSecond+hostEpsilon {
		return fmt.Errorf("Too many checks to this host from this instance. Use a longer interval.")
	}
	return nil
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// toTarget validates the form and returns a Target (without ID).
func (f targetForm) toTarget(defaults *config.Config) (store.Target, error) {
	t := store.Target{Name: f.Name, URL: f.URL, Keyword: f.Keyword, Group: f.Group, Muted: f.Muted,
		Interval: defaults.Interval.Duration, Timeout: defaults.Timeout.Duration}
	if t.URL != "" && !strings.Contains(t.URL, "://") {
		t.URL = "https://" + t.URL
	}
	var err error
	if f.Interval != "" {
		if t.Interval, err = time.ParseDuration(f.Interval); err != nil {
			return t, fmt.Errorf("interval: use a value like 30s or 2m")
		}
	}
	if f.Timeout != "" {
		if t.Timeout, err = time.ParseDuration(f.Timeout); err != nil {
			return t, fmt.Errorf("timeout: use a value like 10s")
		}
	}
	if f.ExpectStatus != "" {
		if t.ExpectStatus, err = strconv.Atoi(f.ExpectStatus); err != nil {
			return t, fmt.Errorf("expected status must be a number")
		}
	}
	if len(t.Group) > 40 {
		return t, fmt.Errorf("group name must be 40 characters or fewer")
	}
	if err := config.ValidateTarget(t.Name, t.URL, t.Interval, t.Timeout, t.ExpectStatus); err != nil {
		return t, err
	}
	return t, nil
}

func (s *Server) targetCreate(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	f := formFromRequest(r, "/targets", "Create monitor")
	fail := func(code int, msg string) {
		if wantsJSON(r) {
			writeJSONStatus(w, code, map[string]any{"error": msg})
			return
		}
		d := monitorsData{base: s.base(r, a, "monitors", "Monitors")}
		d.Error = msg
		d.Rows, _ = s.views(r.Context(), ws, viewOpts{})
		s.renderStatus(w, r, code, "monitors", d)
	}
	if !a.IsOperator {
		if ok, retry := s.createLimiter.Take(wsKey(a)); !ok {
			retryAfter(w, retry)
			fail(http.StatusTooManyRequests, "You're creating monitors too quickly.")
			return
		}
	}
	t, err := f.toTarget(s.cfg)
	if err == nil {
		err = s.checkDestination(a, t.URL)
	}
	if err == nil {
		err = s.limitErr(r.Context(), a, t, 0)
	}
	if err == nil {
		max := 0
		if !a.IsOperator {
			max = *s.cfg.Limits.MaxMonitors
		}
		t, err = ws.CreateTargetCapped(r.Context(), t, s.now(), max)
		if errors.Is(err, store.ErrMonitorCap) {
			err = fmt.Errorf("This workspace has reached its limit of %d monitors.", max)
		}
	}
	if err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := s.mon.Reload(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.resetStatus(ws)
	to := fmt.Sprintf("/targets/%d", t.ID)
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "redirect": withFlash(to, "Monitor created. First check is running.")})
		return
	}
	redirect(w, r, to, "Monitor created. First check is running.")
}

// checkDestination rejects internal hosts early when private targets are off.
func (s *Server) checkDestination(a *authCtx, rawURL string) error {
	if s.cfg.AllowPrivate(a.IsOperator) {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	return netguard.CheckHost(u.Hostname())
}

// targetTest probes the URL in the form without saving anything.
func (s *Server) targetTest(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	if !a.IsOperator {
		if ok, retry := s.testURL.Take(wsKey(a)); !ok {
			retryAfter(w, retry)
			writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"error": "Too many URL tests. Wait a minute and try again."})
			return
		}
	}
	f := formFromRequest(r, "", "")
	if f.Name == "" {
		f.Name = "test"
	}
	t, err := f.toTarget(s.cfg)
	if err == nil {
		err = s.checkDestination(a, t.URL)
	}
	if err != nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if t.Timeout > 25*time.Second {
		t.Timeout = 25 * time.Second
	}
	c := s.mon.Test(r.Context(), ws.ID(), t)
	writeJSON(w, map[string]any{"ok": c.OK, "status_code": c.StatusCode, "latency_ms": c.LatencyMS, "error": c.Error})
}

// ---- target detail ----

type targetData struct {
	base
	View      targetView
	Tab       string
	Form      targetForm
	Range     string
	Ranges    []string
	Chart     template.HTML
	Hours     []cell
	P50, P95  int64
	Incidents []store.Incident
	Checks    []store.Check
}

var ranges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}
var tabs = map[string]bool{"overview": true, "checks": true, "incidents": true, "settings": true}

func (s *Server) targetData(r *http.Request, a *authCtx, t store.Target, st monitor.State) (targetData, error) {
	ctx := r.Context()
	ws := a.WS
	d := targetData{base: s.base(r, a, "monitors", t.Name), Form: formFromTarget(t), Ranges: []string{"24h", "7d", "30d"}}
	d.Tab = r.URL.Query().Get("tab")
	if !tabs[d.Tab] {
		d.Tab = "overview"
	}
	d.Range = r.URL.Query().Get("range")
	span, ok := ranges[d.Range]
	if !ok {
		d.Range, span = "24h", 24*time.Hour
	}
	var err error
	if d.View, err = s.view(ctx, ws, st, viewOpts{}); err != nil {
		return d, err
	}
	now := s.now()
	if d.Incidents, err = ws.ListIncidents(ctx, store.IncidentFilter{TargetID: t.ID, Limit: 100}); err != nil {
		return d, err
	}
	switch d.Tab {
	case "overview":
		checks, err := ws.ChecksSince(ctx, t.ID, now.Add(-span))
		if err != nil {
			return d, err
		}
		d.Chart = latencySVG(checks, d.Incidents, now.Add(-span), now)
		hours, err := ws.HourlyUptime(ctx, t.ID, now, 24)
		if err != nil {
			return d, err
		}
		start := now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
		for i, h := range hours {
			d.Hours = append(d.Hours, uptimeCell(h, start.Add(time.Duration(i)*time.Hour).Local().Format("Jan 2 15:00")))
		}
		lat, err := ws.Latencies(ctx, t.ID, now.Add(-24*time.Hour))
		if err != nil {
			return d, err
		}
		d.P50, d.P95 = store.Percentile(lat, 50), store.Percentile(lat, 95)
		// 11 rows so the template can tell whether an "All checks" link is needed.
		if d.Checks, err = ws.RecentChecks(ctx, t.ID, 11); err != nil {
			return d, err
		}
	case "checks":
		if d.Checks, err = ws.RecentChecks(ctx, t.ID, 200); err != nil {
			return d, err
		}
	}
	return d, nil
}

func (s *Server) loadTarget(w http.ResponseWriter, r *http.Request, ws *store.WS) (store.Target, monitor.State, bool) {
	t, ok := loadByID(s, w, r, ws.GetTarget)
	if !ok {
		return store.Target{}, monitor.State{}, false
	}
	st, ok := s.mon.Get(ws.ID(), t.ID)
	if !ok {
		st = monitor.State{Target: t}
	}
	return t, st, true
}

func (s *Server) targetPage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	t, st, ok := s.loadTarget(w, r, ws)
	if !ok {
		return
	}
	d, err := s.targetData(r, a, t, st)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "target", d)
}

func (s *Server) targetUpdate(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	t, st, ok := s.loadTarget(w, r, ws)
	if !ok {
		return
	}
	f := formFromRequest(r, fmt.Sprintf("/targets/%d", t.ID), "Save changes")
	f.ShowFlags = true
	nt, err := f.toTarget(s.cfg)
	if err == nil {
		err = s.checkDestination(a, nt.URL)
	}
	if err == nil {
		err = s.limitErr(r.Context(), a, nt, t.ID)
	}
	if err == nil {
		nt.ID, nt.Paused, nt.CreatedAt = t.ID, t.Paused, t.CreatedAt
		nt, err = ws.UpdateTarget(r.Context(), nt)
	}
	if err != nil {
		q := r.URL.Query()
		q.Set("tab", "settings")
		r.URL.RawQuery = q.Encode()
		d, derr := s.targetData(r, a, t, st)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		d.Form, d.Error = f, err.Error()
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "target", d)
		return
	}
	if err := s.mon.Reload(r.Context(), nt.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.resetStatus(ws)
	redirect(w, r, fmt.Sprintf("/targets/%d?tab=settings", t.ID), "Monitor saved.")
}

func (s *Server) targetDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	t, _, ok := s.loadTarget(w, r, ws)
	if !ok {
		return
	}
	s.mon.Remove(t.ID) // stops the runner and waits for any in-flight probe
	if err := ws.DeleteTarget(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.resetStatus(ws)
	redirect(w, r, "/targets", "Deleted "+t.Name+".")
}

func (s *Server) targetPause(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	t, _, ok := s.loadTarget(w, r, ws)
	if !ok {
		return
	}
	t.Paused = !t.Paused
	if err := ws.SetPaused(r.Context(), t.ID, t.Paused); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.mon.Reload(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.resetStatus(ws)
	msg := "Resumed " + t.Name + "."
	if t.Paused {
		msg = "Paused " + t.Name + "."
	}
	redirect(w, r, backTo(r, fmt.Sprintf("/targets/%d", t.ID)), msg)
}

func (s *Server) targetMute(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	t, _, ok := s.loadTarget(w, r, ws)
	if !ok {
		return
	}
	t.Muted = !t.Muted
	if err := ws.SetMuted(r.Context(), t.ID, t.Muted); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.mon.Reload(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.resetStatus(ws)
	msg := "Alerts unmuted for " + t.Name + "."
	if t.Muted {
		msg = "Alerts muted for " + t.Name + ". Incidents are still recorded."
	}
	redirect(w, r, backTo(r, fmt.Sprintf("/targets/%d", t.ID)), msg)
}

func (s *Server) targetCheck(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	t, st, ok := s.loadTarget(w, r, ws)
	if !ok {
		return
	}
	if t.Paused {
		if wantsJSON(r) {
			writeJSON(w, map[string]any{"ok": false, "message": t.Name + " is paused."})
			return
		}
		redirect(w, r, fmt.Sprintf("/targets/%d", t.ID), t.Name+" is paused.")
		return
	}
	if !a.IsOperator {
		tk := strconv.FormatInt(t.ID, 10)
		ok, retry := s.checkTarget.Take(tk)
		if ok {
			if ok, retry = s.checkWS.Take(wsKey(a)); !ok {
				s.checkTarget.Undo(tk)
			}
		}
		if !ok {
			retryAfter(w, retry)
			msg := "Too many manual checks. Wait a moment and try again."
			if wantsJSON(r) {
				writeJSONStatus(w, http.StatusTooManyRequests, map[string]any{"ok": false, "message": msg})
				return
			}
			d, err := s.targetData(r, a, t, st)
			if err != nil {
				s.fail(w, err)
				return
			}
			d.Error = msg
			s.renderStatus(w, r, http.StatusTooManyRequests, "target", d)
			return
		}
	}
	msg := "Checking " + t.Name + "…"
	if !s.mon.CheckAsync(ws.ID(), t.ID) {
		msg = "A check is already running."
	}
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "kind": "info", "message": msg})
		return
	}
	redirect(w, r, backTo(r, fmt.Sprintf("/targets/%d", t.ID)), msg)
}
