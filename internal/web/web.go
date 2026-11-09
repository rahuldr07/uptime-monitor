// Package web serves the dashboard, admin pages, public status page and JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/notify"
	"uptimemon/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server holds the HTTP handlers.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	mon      *monitor.Monitor
	logger   *slog.Logger
	now      func() time.Time
	sessions *sessions
	limiter  *loginLimiter
	pages    map[string]*template.Template
	mux      *http.ServeMux
}

// New builds a Server. now may be nil.
func New(cfg *config.Config, st *store.Store, mon *monitor.Monitor, logger *slog.Logger, now func() time.Time) (*Server, error) {
	if now == nil {
		now = time.Now
	}
	s := &Server{cfg: cfg, store: st, mon: mon, logger: logger, now: now,
		sessions: newSessions(now), limiter: newLoginLimiter(now), pages: map[string]*template.Template{}}
	funcs := template.FuncMap{
		"ago":    func(t time.Time) string { return humanAgo(s.now().Sub(t)) },
		"dur":    humanDur,
		"durstr": func(d time.Duration) string { return shortDur(d) },
		"ts":     formatTS,
		"pct": func(st store.Stats) string {
			if st.Total == 0 {
				return "—"
			}
			f := st.UptimePercent()
			if f >= 99.995 {
				return "100%"
			}
			return fmt.Sprintf("%.2f%%", f)
		},
		"ms": func(st store.Stats) string {
			if st.OKCount == 0 {
				return "—"
			}
			return fmt.Sprintf("%d ms", int64(math.Round(st.AvgLatencyMS)))
		},
		"msn": func(v int64) string {
			if v == 0 {
				return "—"
			}
			return fmt.Sprintf("%d ms", v)
		},
		"min": func(a, b int) int {
			if a < b {
				return a
			}
			return b
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
	}
	shared := []string{"templates/icons.html", "templates/form.html", "templates/partials.html"}
	for _, page := range []string{"overview", "monitors", "target", "incidents", "alerts"} {
		files := append([]string{"templates/layout.html"}, shared...)
		files = append(files, "templates/"+page+".html")
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		s.pages[page] = t
	}
	for _, page := range []string{"status", "login"} {
		files := append([]string{"templates/public.html"}, shared...)
		files = append(files, "templates/"+page+".html")
		t, err := template.New("public.html").Funcs(funcs).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		s.pages[page] = t
	}
	s.routes()
	return s, nil
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	m.HandleFunc("GET /status", s.statusPage)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", sameOrigin(s.loginPost))
	m.HandleFunc("POST /logout", sameOrigin(s.logout))

	admin := func(h http.HandlerFunc) http.HandlerFunc { return s.requireAdmin(h) }
	post := func(h http.HandlerFunc) http.HandlerFunc { return sameOrigin(s.requireAdmin(h)) }
	m.HandleFunc("GET /{$}", admin(s.overview))
	m.HandleFunc("GET /events", admin(s.events))
	m.HandleFunc("GET /targets", admin(s.monitorsPage))
	m.HandleFunc("POST /targets", post(s.targetCreate))
	m.HandleFunc("POST /targets/test", post(s.targetTest))
	m.HandleFunc("GET /targets/{id}", admin(s.targetPage))
	m.HandleFunc("POST /targets/{id}", post(s.targetUpdate))
	m.HandleFunc("POST /targets/{id}/delete", post(s.targetDelete))
	m.HandleFunc("POST /targets/{id}/pause", post(s.targetPause))
	m.HandleFunc("POST /targets/{id}/mute", post(s.targetMute))
	m.HandleFunc("POST /targets/{id}/check", post(s.targetCheck))
	m.HandleFunc("GET /incidents", admin(s.incidentsPage))
	m.HandleFunc("POST /incidents/{id}/ack", post(s.incidentAck))
	m.HandleFunc("POST /incidents/{id}/note", post(s.incidentNote))
	m.HandleFunc("GET /alerts", admin(s.alertsPage))
	m.HandleFunc("POST /alerts", post(s.alertsSave))
	m.HandleFunc("POST /alerts/test", post(s.alertsTest))
	m.HandleFunc("POST /status/settings", post(s.statusSettings))
	m.HandleFunc("GET /api/status", admin(s.apiStatus))
	m.HandleFunc("GET /api/targets/{id}/checks", admin(s.apiChecks))
	m.HandleFunc("GET /api/incidents", admin(s.apiIncidents))
	s.mux = m
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		h.ServeHTTP(w, r)
	})
}

// ---- view models ----

type base struct {
	Title       string
	Page        string
	Admin       bool
	AuthEnabled bool
	Flash       string
	Error       string
	Now         time.Time
	Theme       string
	Unacked     int
	DownCount   int
	NewForm     targetForm
	Groups      []string
}

func (s *Server) base(r *http.Request, page, title string) base {
	b := base{Title: title, Page: page, Admin: s.isAdmin(r), AuthEnabled: s.authEnabled(), Now: s.now(), Theme: themeFrom(r)}
	if b.Admin {
		// Only admin flows redirect with a flash; never echo ?msg= to anonymous visitors.
		b.Flash = r.URL.Query().Get("msg")
	}
	b.NewForm = targetForm{Action: "/targets", Submit: "Create monitor", ShowTest: true,
		Interval: shortDur(s.cfg.Interval.Duration), Timeout: shortDur(s.cfg.Timeout.Duration)}
	if b.Admin {
		b.Unacked, _ = s.store.CountUnacknowledged(r.Context())
	}
	for _, st := range s.mon.Snapshot() {
		if st.Status == monitor.Down {
			b.DownCount++
		}
		if g := st.Target.Group; g != "" && !contains(b.Groups, g) {
			b.Groups = append(b.Groups, g)
		}
	}
	sort.Strings(b.Groups)
	return b
}

func themeFrom(r *http.Request) string {
	if c, err := r.Cookie("theme"); err == nil && (c.Value == "dark" || c.Value == "light") {
		return c.Value
	}
	return ""
}

type beat struct {
	Class  string
	Height int
	Title  string
}

type cell struct {
	Class string
	Title string
}

type targetView struct {
	State     monitor.State
	Class     string
	Label     string
	DownFor   time.Duration
	Heartbeat []beat
	Spark     template.HTML
	Days      []cell
	Up24      store.Stats
	Up7       store.Stats
	Up30      store.Stats
}

type viewOpts struct {
	days  bool
	spark bool
}

func (s *Server) view(ctx context.Context, st monitor.State, o viewOpts) (targetView, error) {
	now := s.now()
	label := st.Status.String()
	v := targetView{State: st, Class: label, Label: strings.ToUpper(label[:1]) + label[1:]}
	switch st.Status {
	case monitor.Down:
		v.DownFor = now.Sub(st.Since)
		v.Label = "Down " + humanDur(v.DownFor)
	case monitor.Unknown:
		v.Label = "Pending"
	}
	var err error
	if v.Up24, err = s.store.Stats(ctx, st.Target.ID, now.Add(-24*time.Hour)); err != nil {
		return v, err
	}
	if v.Up7, err = s.store.Stats(ctx, st.Target.ID, now.Add(-7*24*time.Hour)); err != nil {
		return v, err
	}
	if v.Up30, err = s.store.Stats(ctx, st.Target.ID, now.Add(-30*24*time.Hour)); err != nil {
		return v, err
	}
	if o.spark {
		checks, err := s.store.ChecksSince(ctx, st.Target.ID, now.Add(-30*time.Minute))
		if err != nil {
			return v, err
		}
		v.Spark = sparkSVG(checks, now.Add(-30*time.Minute), now)
	}
	if o.days {
		days, err := s.store.DailyUptime(ctx, st.Target.ID, now, 90)
		if err != nil {
			return v, err
		}
		start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -89)
		for i, d := range days {
			v.Days = append(v.Days, uptimeCell(d, start.AddDate(0, 0, i).Format("Jan 2")))
		}
	}
	return v, nil
}

func uptimeCell(d store.Stats, label string) cell {
	switch {
	case d.Total == 0:
		return cell{"none", label + " · no data"}
	case d.OKCount == d.Total:
		return cell{"ok", label + " · 100%"}
	case d.UptimePercent() < 90:
		return cell{"bad", fmt.Sprintf("%s · %.1f%%", label, d.UptimePercent())}
	default:
		return cell{"part", fmt.Sprintf("%s · %.2f%%", label, d.UptimePercent())}
	}
}

func (s *Server) views(ctx context.Context, o viewOpts) ([]targetView, error) {
	states := s.mon.Snapshot()
	out := make([]targetView, 0, len(states))
	for _, st := range states {
		v, err := s.view(ctx, st, o)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

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
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, "monitors", d)
		return
	}
	if err := s.mon.Reload(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	to := fmt.Sprintf("/targets/%d", t.ID)
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "redirect": to + "?msg=" + url.QueryEscape("Monitor created. First check is running.")})
		return
	}
	redirect(w, r, to, "Monitor created. First check is running.")
}

// targetTest probes the URL in the form without saving anything.
func (s *Server) targetTest(w http.ResponseWriter, r *http.Request) {
	f := formFromRequest(r, "", "")
	if f.Name == "" {
		f.Name = "test"
	}
	t, err := f.toTarget(s.cfg)
	if err != nil {
		writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error()})
		return
	}
	if t.Timeout > 25*time.Second {
		t.Timeout = 25 * time.Second
	}
	c := monitor.Probe(r.Context(), &http.Client{}, t, s.now)
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
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Target{}, monitor.State{}, false
	}
	t, err := s.store.GetTarget(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Target{}, monitor.State{}, false
	}
	if err != nil {
		s.fail(w, err)
		return store.Target{}, monitor.State{}, false
	}
	st, ok := s.mon.Get(id)
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
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, r, "target", d)
		return
	}
	if err := s.mon.Reload(r.Context(), nt); err != nil {
		s.fail(w, err)
		return
	}
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

// ---- incidents ----

type incidentsData struct {
	base
	Targets   []store.Target
	Filter    store.IncidentFilter
	Incidents []store.Incident
}

func (s *Server) incidentsPage(w http.ResponseWriter, r *http.Request) {
	d := incidentsData{base: s.base(r, "incidents", "Incidents")}
	d.Filter.TargetID, _ = strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	d.Filter.OngoingOnly = r.URL.Query().Get("ongoing") == "1"
	var err error
	if d.Targets, err = s.store.ListTargets(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	if d.Incidents, err = s.store.ListIncidents(r.Context(), d.Filter); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "incidents", d)
}

func (s *Server) loadIncident(w http.ResponseWriter, r *http.Request) (store.Incident, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return store.Incident{}, false
	}
	in, err := s.store.GetIncident(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return store.Incident{}, false
	}
	if err != nil {
		s.fail(w, err)
		return store.Incident{}, false
	}
	return in, true
}

func (s *Server) incidentAck(w http.ResponseWriter, r *http.Request) {
	in, ok := s.loadIncident(w, r)
	if !ok {
		return
	}
	if err := s.store.AcknowledgeIncident(r.Context(), in.ID, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, map[string]any{"ok": true, "message": "Acknowledged " + in.TargetName + " incident."})
		return
	}
	redirect(w, r, backTo(r, "/incidents"), "Acknowledged.")
}

func (s *Server) incidentNote(w http.ResponseWriter, r *http.Request) {
	in, ok := s.loadIncident(w, r)
	if !ok {
		return
	}
	note := strings.TrimSpace(r.FormValue("note"))
	if utf8.RuneCountInString(note) > 500 {
		redirect(w, r, backTo(r, "/incidents"), "Note must be 500 characters or fewer.")
		return
	}
	if err := s.store.SetIncidentNote(r.Context(), in.ID, note); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, backTo(r, "/incidents"), "Note saved.")
}

// ---- alerts ----

type alertsData struct {
	base
	N         config.Notify
	To        string
	Has       map[string]bool
	Last      map[string]store.Alert
	Threshold int
}

// notifyConfig returns the stored notify settings, falling back to the config file.
func (s *Server) notifyConfig(ctx context.Context) config.Notify {
	raw, err := s.store.GetSetting(ctx, "notify")
	if err == nil {
		var n config.Notify
		if json.Unmarshal([]byte(raw), &n) == nil {
			return n
		}
	}
	return s.cfg.Notify
}

func (s *Server) alertsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAlerts(w, r, s.notifyConfig(r.Context()), "")
}

func (s *Server) renderAlerts(w http.ResponseWriter, r *http.Request, n config.Notify, errMsg string) {
	d := alertsData{base: s.base(r, "alerts", "Alert channels"), N: n, Has: map[string]bool{}, Threshold: s.cfg.FailureThreshold}
	d.Error = errMsg
	if d.N.Telegram == nil {
		d.N.Telegram = &config.Telegram{}
	}
	if d.N.Slack == nil {
		d.N.Slack = &config.Slack{}
	}
	if d.N.Email == nil {
		d.N.Email = &config.Email{}
	}
	d.To = strings.Join(d.N.Email.To, ", ")
	for _, nn := range s.mon.Notifiers() {
		d.Has[nn.Name] = true
	}
	d.Last, _ = s.store.LastAlerts(r.Context())
	s.render(w, r, "alerts", d)
}

// parseNotifyForm builds notify settings from the alerts form.
func parseNotifyForm(r *http.Request) config.Notify {
	v := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	var n config.Notify
	if v("tg_token") != "" || v("tg_chat") != "" {
		n.Telegram = &config.Telegram{BotToken: v("tg_token"), ChatID: v("tg_chat"), Disabled: v("tg_enabled") != "1"}
	}
	if v("slack_url") != "" {
		n.Slack = &config.Slack{WebhookURL: v("slack_url"), Disabled: v("slack_enabled") != "1"}
	}
	if v("smtp_host") != "" || v("smtp_from") != "" || v("smtp_to") != "" {
		port, _ := strconv.Atoi(v("smtp_port"))
		var to []string
		for _, a := range strings.Split(v("smtp_to"), ",") {
			if a = strings.TrimSpace(a); a != "" {
				to = append(to, a)
			}
		}
		n.Email = &config.Email{SMTPHost: v("smtp_host"), SMTPPort: port, Username: v("smtp_user"),
			Password: v("smtp_pass"), From: v("smtp_from"), To: to, Disabled: v("email_enabled") != "1"}
	}
	return n
}

// saveNotify validates and persists settings, swapping the live notifiers.
func (s *Server) saveNotify(w http.ResponseWriter, r *http.Request) (config.Notify, bool) {
	n := parseNotifyForm(r)
	if err := config.ValidateNotify(&n); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.renderAlerts(w, r, n, err.Error())
		return n, false
	}
	raw, _ := json.Marshal(n)
	if err := s.store.SetSetting(r.Context(), "notify", string(raw)); err != nil {
		s.fail(w, err)
		return n, false
	}
	s.mon.SetNotifiers(notify.FromConfig(n, s.logger))
	return n, true
}

func (s *Server) alertsSave(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.saveNotify(w, r); !ok {
		return
	}
	redirect(w, r, "/alerts", "Alert channels saved.")
}

// alertsTest saves the form first, then sends a test through one channel.
func (s *Server) alertsTest(w http.ResponseWriter, r *http.Request) {
	n, ok := s.saveNotify(w, r)
	if !ok {
		return
	}
	ch := r.FormValue("channel")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.mon.SendTest(ctx, ch); err != nil {
		s.logger.Warn("test alert failed", "channel", ch, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		s.renderAlerts(w, r, n, "Settings saved, but the test to "+ch+" failed: "+err.Error())
		return
	}
	redirect(w, r, "/alerts", "Settings saved and test alert sent to "+ch+".")
}

// ---- status page ----

type statusGroup struct {
	Name    string
	Targets []targetView
}

type statusData struct {
	base
	StatusTitle string
	StatusDesc  string
	ShareURL    string
	Groups      []statusGroup
	Total       int
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	d := statusData{base: s.base(r, "status", "Status")}
	d.StatusTitle, d.StatusDesc = s.statusMeta(r.Context())
	d.Title = d.StatusTitle
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	d.ShareURL = scheme + "://" + r.Host + "/status"
	views, err := s.views(r.Context(), viewOpts{days: true})
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Total = len(views)
	byGroup := map[string]*statusGroup{}
	for _, v := range views {
		g := v.State.Target.Group
		if byGroup[g] == nil {
			byGroup[g] = &statusGroup{Name: g}
		}
		byGroup[g].Targets = append(byGroup[g].Targets, v)
	}
	for _, g := range byGroup {
		d.Groups = append(d.Groups, *g)
	}
	sort.Slice(d.Groups, func(i, j int) bool {
		if (d.Groups[i].Name == "") != (d.Groups[j].Name == "") {
			return d.Groups[i].Name == "" // ungrouped first
		}
		return d.Groups[i].Name < d.Groups[j].Name
	})
	s.render(w, r, "status", d)
}

func (s *Server) statusMeta(ctx context.Context) (title, desc string) {
	title, _ = s.store.GetSetting(ctx, "status_title")
	desc, _ = s.store.GetSetting(ctx, "status_desc")
	if title == "" {
		title = "Service status"
	}
	return title, desc
}

func (s *Server) statusSettings(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.FormValue("title"))
	desc := strings.TrimSpace(r.FormValue("description"))
	if len(title) > 80 || len(desc) > 200 {
		redirect(w, r, "/status", "Title or description too long.")
		return
	}
	if err := s.store.SetSetting(r.Context(), "status_title", title); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.store.SetSetting(r.Context(), "status_desc", desc); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, "/status", "Status page saved.")
}

// ---- live events ----

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	// The server's WriteTimeout would otherwise cut this long-lived stream.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		s.logger.Warn("sse: cannot clear write deadline", "err", err)
	}
	ch, unsub := s.mon.Events.Subscribe()
	defer unsub()
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// ---- auth pages ----

type loginData struct {
	base
	Next string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.isAdmin(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", loginData{base: s.base(r, "login", "Log in · Uptime"), Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := loginData{base: s.base(r, "login", "Log in · Uptime"), Next: safeNext(r.FormValue("next"))}
	key := clientKey(r, s.cfg.TrustProxy)
	if s.limiter.blocked(key) {
		d.Error = "Too many failed attempts. Wait a minute and try again."
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login", d)
		return
	}
	if passwordEqual(r.FormValue("password"), s.cfg.AdminPassword) {
		s.setSession(w, r)
		http.Redirect(w, r, d.Next, http.StatusSeeOther)
		return
	}
	s.limiter.fail(key)
	d.Error = "Wrong password."
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, r, "login", d)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w, r)
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

// ---- JSON API ----

type apiTarget struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	URL         string     `json:"url"`
	Group       string     `json:"group,omitempty"`
	Status      string     `json:"status"`
	Since       *time.Time `json:"since,omitempty"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	LatencyMS   *int64     `json:"latency_ms,omitempty"`
	StatusCode  int        `json:"status_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	Uptime24h   float64    `json:"uptime_24h"`
	Uptime7d    float64    `json:"uptime_7d"`
	Paused      bool       `json:"paused"`
	Muted       bool       `json:"muted"`
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context(), viewOpts{})
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]apiTarget, 0, len(views))
	for _, v := range views {
		st := v.State
		a := apiTarget{ID: st.Target.ID, Name: st.Target.Name, URL: st.Target.URL, Group: st.Target.Group, Status: st.Status.String(),
			Uptime24h: round2(v.Up24.UptimePercent()), Uptime7d: round2(v.Up7.UptimePercent()), Paused: st.Target.Paused, Muted: st.Target.Muted}
		if !st.Since.IsZero() {
			t := st.Since
			a.Since = &t
		}
		if st.Last != nil {
			t := st.Last.CheckedAt
			a.LastChecked = &t
			l := st.Last.LatencyMS
			a.LatencyMS = &l
			a.StatusCode = st.Last.StatusCode
			a.Error = st.Last.Error
		}
		out = append(out, a)
	}
	writeJSON(w, map[string]any{"generated_at": s.now().UTC(), "targets": out})
}

func (s *Server) apiChecks(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	span, ok := ranges[r.URL.Query().Get("range")]
	if !ok {
		span = 24 * time.Hour
	}
	checks, err := s.store.ChecksSince(r.Context(), t.ID, s.now().Add(-span))
	if err != nil {
		s.fail(w, err)
		return
	}
	type c struct {
		At         time.Time `json:"at"`
		OK         bool      `json:"ok"`
		StatusCode int       `json:"status_code"`
		LatencyMS  int64     `json:"latency_ms"`
		Error      string    `json:"error,omitempty"`
	}
	out := make([]c, 0, len(checks))
	for _, ch := range checks {
		out = append(out, c{ch.CheckedAt, ch.OK, ch.StatusCode, ch.LatencyMS, ch.Error})
	}
	writeJSON(w, map[string]any{"target": t.Name, "checks": out})
}

func (s *Server) apiIncidents(w http.ResponseWriter, r *http.Request) {
	var f store.IncidentFilter
	f.TargetID, _ = strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	f.OngoingOnly = r.URL.Query().Get("ongoing") == "1"
	list, err := s.store.ListIncidents(r.Context(), f)
	if err != nil {
		s.fail(w, err)
		return
	}
	type in struct {
		ID           int64      `json:"id"`
		Target       string     `json:"target"`
		TargetID     int64      `json:"target_id"`
		StartedAt    time.Time  `json:"started_at"`
		EndedAt      *time.Time `json:"ended_at,omitempty"`
		DurationS    int64      `json:"duration_s"`
		Cause        string     `json:"cause"`
		AlertedVia   string     `json:"alerted_via"`
		Acknowledged *time.Time `json:"acknowledged_at,omitempty"`
		Note         string     `json:"note,omitempty"`
	}
	now := s.now()
	out := make([]in, 0, len(list))
	for _, i := range list {
		out = append(out, in{i.ID, i.TargetName, i.TargetID, i.StartedAt, i.EndedAt, int64(i.Duration(now).Seconds()), i.Cause, i.Channels, i.AckedAt, i.Note})
	}
	writeJSON(w, map[string]any{"incidents": out})
}

// ---- helpers ----

// render writes a full page, or only the content block for live refreshes.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := "layout"
	if r.Header.Get("X-Partial") == "1" {
		name = "content"
	}
	if err := s.pages[page].ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("render", "page", page, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func redirect(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// backTo returns the Referer path and query (minus any old flash) when it
// is a local page, so filters and tabs survive a round trip; else fallback.
func backTo(r *http.Request, fallback string) string {
	ref, err := url.Parse(r.Referer())
	if err != nil || ref.Host != r.Host || !strings.HasPrefix(ref.Path, "/") {
		return fallback
	}
	q := ref.Query()
	q.Del("msg")
	if len(q) > 0 {
		return ref.Path + "?" + q.Encode()
	}
	return ref.Path
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// shortDur renders durations the way people type them: 30s, 2m, 1h30m.
func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if sec > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", sec)
	}
	return b.String()
}

func humanAgo(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

func formatTS(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "—"
		}
		t = *x
	default:
		return "—"
	}
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("Jan 2 15:04:05")
}
