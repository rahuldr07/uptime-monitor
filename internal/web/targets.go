package web

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/netguard"
	"uptimemon/internal/store"
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

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := overviewData{base: s.base(r, "overview", "Overview")}
	views, err := s.views(ctx, viewOpts{spark: true})
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
	if d.Overall24, err = s.store.Stats(ctx, 0, now.Add(-24*time.Hour)); err != nil {
		s.fail(w, err)
		return
	}
	lat, err := s.store.Latencies(ctx, 0, now.Add(-24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	d.P95 = store.Percentile(lat, 95)
	if d.Incidents7, err = s.store.CountIncidents(ctx, now.Add(-7*24*time.Hour)); err != nil {
		s.fail(w, err)
		return
	}
	if d.Alerts7, err = s.store.CountAlerts(ctx, now.Add(-7*24*time.Hour)); err != nil {
		s.fail(w, err)
		return
	}
	for _, n := range s.mon.Notifiers() {
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

func (s *Server) monitorsPage(w http.ResponseWriter, r *http.Request) {
	d := monitorsData{base: s.base(r, "monitors", "Monitors")}
	var err error
	if d.Rows, err = s.views(r.Context(), viewOpts{}); err != nil {
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

func (s *Server) targetCreate(w http.ResponseWriter, r *http.Request) {
	f := formFromRequest(r, "/targets", "Create monitor")
	t, err := f.toTarget(s.cfg)
	if err == nil {
		err = s.checkDestination(t.URL)
	}
	if err == nil {
		t, err = s.store.CreateTarget(r.Context(), t, s.now())
	}
	if err != nil {
		if wantsJSON(r) {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
			return
		}
		d := monitorsData{base: s.base(r, "monitors", "Monitors")}
		d.Error = err.Error()
		d.Rows, _ = s.views(r.Context(), viewOpts{})
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "monitors", d)
		return
	}
	if err := s.mon.Reload(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	s.statusCache.reset()
	to := fmt.Sprintf("/targets/%d", t.ID)
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "redirect": withFlash(to, "Monitor created. First check is running.")})
		return
	}
	redirect(w, r, to, "Monitor created. First check is running.")
}

// checkDestination rejects internal hosts early when private targets are off.
func (s *Server) checkDestination(rawURL string) error {
	if s.cfg.AllowPrivate(true) {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	return netguard.CheckHost(u.Hostname())
}

// targetTest probes the URL in the form without saving anything.
func (s *Server) targetTest(w http.ResponseWriter, r *http.Request) {
	f := formFromRequest(r, "", "")
	if f.Name == "" {
		f.Name = "test"
	}
	t, err := f.toTarget(s.cfg)
	if err == nil {
		err = s.checkDestination(t.URL)
	}
	if err != nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if t.Timeout > 25*time.Second {
		t.Timeout = 25 * time.Second
	}
	c := s.mon.Test(r.Context(), t)
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

func (s *Server) targetData(r *http.Request, t store.Target, st monitor.State) (targetData, error) {
	ctx := r.Context()
	d := targetData{base: s.base(r, "monitors", t.Name), Form: formFromTarget(t), Ranges: []string{"24h", "7d", "30d"}}
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
	if d.View, err = s.view(ctx, st, viewOpts{}); err != nil {
		return d, err
	}
	now := s.now()
	if d.Incidents, err = s.store.ListIncidents(ctx, store.IncidentFilter{TargetID: t.ID, Limit: 100}); err != nil {
		return d, err
	}
	switch d.Tab {
	case "overview":
		checks, err := s.store.ChecksSince(ctx, t.ID, now.Add(-span))
		if err != nil {
			return d, err
		}
		d.Chart = latencySVG(checks, d.Incidents, now.Add(-span), now)
		hours, err := s.store.HourlyUptime(ctx, t.ID, now, 24)
		if err != nil {
			return d, err
		}
		start := now.UTC().Truncate(time.Hour).Add(-23 * time.Hour)
		for i, h := range hours {
			d.Hours = append(d.Hours, uptimeCell(h, start.Add(time.Duration(i)*time.Hour).Local().Format("Jan 2 15:00")))
		}
		lat, err := s.store.Latencies(ctx, t.ID, now.Add(-24*time.Hour))
		if err != nil {
			return d, err
		}
		d.P50, d.P95 = store.Percentile(lat, 50), store.Percentile(lat, 95)
		// 11 rows so the template can tell whether an "All checks" link is needed.
		if d.Checks, err = s.store.RecentChecks(ctx, t.ID, 11); err != nil {
			return d, err
		}
	case "checks":
		if d.Checks, err = s.store.RecentChecks(ctx, t.ID, 200); err != nil {
			return d, err
		}
	}
	return d, nil
}

func (s *Server) loadTarget(w http.ResponseWriter, r *http.Request) (store.Target, monitor.State, bool) {
	t, ok := loadByID(s, w, r, s.store.GetTarget)
	if !ok {
		return store.Target{}, monitor.State{}, false
	}
	st, ok := s.mon.Get(t.ID)
	if !ok {
		st = monitor.State{Target: t}
	}
	return t, st, true
}

func (s *Server) targetPage(w http.ResponseWriter, r *http.Request) {
	t, st, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	d, err := s.targetData(r, t, st)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "target", d)
}

func (s *Server) targetUpdate(w http.ResponseWriter, r *http.Request) {
	t, st, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	f := formFromRequest(r, fmt.Sprintf("/targets/%d", t.ID), "Save changes")
	f.ShowFlags = true
	nt, err := f.toTarget(s.cfg)
	if err == nil {
		err = s.checkDestination(nt.URL)
	}
	if err == nil {
		nt.ID, nt.Paused, nt.CreatedAt = t.ID, t.Paused, t.CreatedAt
		err = s.store.UpdateTarget(r.Context(), nt)
	}
	if err != nil {
		q := r.URL.Query()
		q.Set("tab", "settings")
		r.URL.RawQuery = q.Encode()
		d, derr := s.targetData(r, t, st)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		d.Form, d.Error = f, err.Error()
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "target", d)
		return
	}
	if err := s.mon.Reload(r.Context(), nt); err != nil {
		s.fail(w, err)
		return
	}
	s.statusCache.reset()
	redirect(w, r, fmt.Sprintf("/targets/%d?tab=settings", t.ID), "Monitor saved.")
}

func (s *Server) targetDelete(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	s.mon.Remove(t.ID) // stops the runner and waits for any in-flight probe
	if err := s.store.DeleteTarget(r.Context(), t.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.statusCache.reset()
	redirect(w, r, "/targets", "Deleted "+t.Name+".")
}

func (s *Server) targetPause(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	t.Paused = !t.Paused
	if err := s.store.SetPaused(r.Context(), t.ID, t.Paused); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.mon.Reload(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	s.statusCache.reset()
	msg := "Resumed " + t.Name + "."
	if t.Paused {
		msg = "Paused " + t.Name + "."
	}
	redirect(w, r, backTo(r, fmt.Sprintf("/targets/%d", t.ID)), msg)
}

func (s *Server) targetMute(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	t.Muted = !t.Muted
	if err := s.store.SetMuted(r.Context(), t.ID, t.Muted); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.mon.Reload(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	msg := "Alerts unmuted for " + t.Name + "."
	if t.Muted {
		msg = "Alerts muted for " + t.Name + ". Incidents are still recorded."
	}
	redirect(w, r, backTo(r, fmt.Sprintf("/targets/%d", t.ID)), msg)
}

func (s *Server) targetCheck(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
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
	s.mon.CheckAsync(t.ID)
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "message": "Checking " + t.Name + "…"})
		return
	}
	redirect(w, r, backTo(r, fmt.Sprintf("/targets/%d", t.ID)), "Checking "+t.Name+"…")
}
