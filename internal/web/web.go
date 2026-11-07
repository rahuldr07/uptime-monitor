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
	"strconv"
	"strings"
	"time"

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
		"durstr": func(d time.Duration) string { return d.String() },
		"ts":     func(t any) string { return formatTS(t) },
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
	}
	for _, page := range []string{"dashboard", "status", "target", "targets", "incidents", "alerts", "login"} {
		files := []string{"templates/layout.html", "templates/" + page + ".html"}
		if page == "target" || page == "targets" {
			files = append(files, "templates/form.html")
		}
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, files...)
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

	m.HandleFunc("GET /{$}", s.requireAdmin(s.dashboard))
	m.HandleFunc("GET /partials/dashboard", s.requireAdmin(s.dashboardPartial))
	m.HandleFunc("GET /targets", s.requireAdmin(s.targetsPage))
	m.HandleFunc("POST /targets", sameOrigin(s.requireAdmin(s.targetCreate)))
	m.HandleFunc("GET /targets/{id}", s.requireAdmin(s.targetPage))
	m.HandleFunc("POST /targets/{id}", sameOrigin(s.requireAdmin(s.targetUpdate)))
	m.HandleFunc("POST /targets/{id}/delete", sameOrigin(s.requireAdmin(s.targetDelete)))
	m.HandleFunc("POST /targets/{id}/pause", sameOrigin(s.requireAdmin(s.targetPause)))
	m.HandleFunc("POST /targets/{id}/check", sameOrigin(s.requireAdmin(s.targetCheck)))
	m.HandleFunc("GET /incidents", s.requireAdmin(s.incidentsPage))
	m.HandleFunc("GET /alerts", s.requireAdmin(s.alertsPage))
	m.HandleFunc("POST /alerts", sameOrigin(s.requireAdmin(s.alertsSave)))
	m.HandleFunc("POST /alerts/test", sameOrigin(s.requireAdmin(s.alertsTest)))
	m.HandleFunc("GET /api/status", s.requireAdmin(s.apiStatus))
	m.HandleFunc("GET /api/targets/{id}/checks", s.requireAdmin(s.apiChecks))
	m.HandleFunc("GET /api/incidents", s.requireAdmin(s.apiIncidents))
	s.mux = m
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
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
}

func (s *Server) base(r *http.Request, page, title string) base {
	return base{Title: title, Page: page, Admin: s.isAdmin(r), AuthEnabled: s.authEnabled(),
		Flash: r.URL.Query().Get("msg"), Now: s.now()}
}

type beat struct {
	Class  string
	Height int
	Title  string
}

type day struct {
	Class string
	Title string
}

type targetView struct {
	State     monitor.State
	Class     string
	Label     string
	DownFor   time.Duration
	Heartbeat []beat
	Days      []day
	Up24      store.Stats
	Up7       store.Stats
	Up30      store.Stats
}

func (s *Server) view(ctx context.Context, st monitor.State, withDays bool) (targetView, error) {
	now := s.now()
	v := targetView{State: st, Class: st.Status.String(), Label: strings.ToUpper(st.Status.String()[:1]) + st.Status.String()[1:]}
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
	recent, err := s.store.RecentChecks(ctx, st.Target.ID, 48)
	if err != nil {
		return v, err
	}
	var maxMS int64 = 1
	for _, c := range recent {
		if c.OK && c.LatencyMS > maxMS {
			maxMS = c.LatencyMS
		}
	}
	v.Heartbeat = make([]beat, 0, 48)
	for i := 48 - len(recent); i > 0; i-- {
		v.Heartbeat = append(v.Heartbeat, beat{Class: "none"})
	}
	for i := len(recent) - 1; i >= 0; i-- { // oldest first
		c := recent[i]
		if c.OK {
			h := 30 + int(70*float64(c.LatencyMS)/float64(maxMS))
			v.Heartbeat = append(v.Heartbeat, beat{Class: "ok", Height: h, Title: fmt.Sprintf("%s · %d ms", formatTS(c.CheckedAt), c.LatencyMS)})
		} else {
			v.Heartbeat = append(v.Heartbeat, beat{Class: "bad", Height: 100, Title: fmt.Sprintf("%s · %s", formatTS(c.CheckedAt), c.Error)})
		}
	}
	if withDays {
		days, err := s.store.DailyUptime(ctx, st.Target.ID, now, 90)
		if err != nil {
			return v, err
		}
		start := now.UTC().Truncate(24*time.Hour).AddDate(0, 0, -89)
		for i, d := range days {
			date := start.AddDate(0, 0, i).Format("Jan 2")
			switch {
			case d.Total == 0:
				v.Days = append(v.Days, day{"none", date + " · no data"})
			case d.OKCount == d.Total:
				v.Days = append(v.Days, day{"ok", date + " · 100%"})
			case d.UptimePercent() < 90:
				v.Days = append(v.Days, day{"bad", fmt.Sprintf("%s · %.1f%%", date, d.UptimePercent())})
			default:
				v.Days = append(v.Days, day{"part", fmt.Sprintf("%s · %.2f%%", date, d.UptimePercent())})
			}
		}
	}
	return v, nil
}

func (s *Server) views(ctx context.Context, withDays bool) ([]targetView, error) {
	states := s.mon.Snapshot()
	out := make([]targetView, 0, len(states))
	for _, st := range states {
		v, err := s.view(ctx, st, withDays)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// ---- pages ----

type dashboardData struct {
	base
	Targets     []targetView
	Total       int
	DownCount   int
	Unknown     int
	LongestDown *targetView
	Overall24   store.Stats
	Incidents7  int
	Alerts7     int
}

func (s *Server) dashboardData(r *http.Request) (dashboardData, error) {
	ctx := r.Context()
	d := dashboardData{base: s.base(r, "dashboard", "Dashboard")}
	views, err := s.views(ctx, false)
	if err != nil {
		return d, err
	}
	d.Targets = views
	d.Total = len(views)
	for i := range views {
		switch views[i].State.Status {
		case monitor.Down:
			d.DownCount++
			if d.LongestDown == nil || views[i].DownFor > d.LongestDown.DownFor {
				d.LongestDown = &views[i]
			}
		case monitor.Unknown:
			d.Unknown++
		}
	}
	now := s.now()
	if d.Overall24, err = s.store.Stats(ctx, 0, now.Add(-24*time.Hour)); err != nil {
		return d, err
	}
	if d.Incidents7, err = s.store.CountIncidents(ctx, now.Add(-7*24*time.Hour)); err != nil {
		return d, err
	}
	if d.Alerts7, err = s.store.CountAlerts(ctx, now.Add(-7*24*time.Hour)); err != nil {
		return d, err
	}
	return d, nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := s.dashboardData(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "dashboard", "layout", d)
}

func (s *Server) dashboardPartial(w http.ResponseWriter, r *http.Request) {
	d, err := s.dashboardData(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "dashboard", "dashboard-body", d)
}

type statusData struct {
	base
	Targets   []targetView
	Total     int
	DownCount int
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	d := statusData{base: s.base(r, "status", "Status")}
	views, err := s.views(r.Context(), true)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Targets = views
	d.Total = len(views)
	for _, v := range views {
		if v.State.Status == monitor.Down {
			d.DownCount++
		}
	}
	s.render(w, "status", "layout", d)
}

type targetForm struct {
	Action       string
	Submit       string
	Name         string
	URL          string
	Interval     string
	Timeout      string
	ExpectStatus string
	Keyword      string
}

func formFromTarget(t store.Target) targetForm {
	f := targetForm{Action: fmt.Sprintf("/targets/%d", t.ID), Submit: "Save changes", Name: t.Name, URL: t.URL,
		Interval: t.Interval.String(), Timeout: t.Timeout.String(), Keyword: t.Keyword}
	if t.ExpectStatus != 0 {
		f.ExpectStatus = strconv.Itoa(t.ExpectStatus)
	}
	return f
}

func formFromRequest(r *http.Request, action, submit string) targetForm {
	return targetForm{Action: action, Submit: submit,
		Name: strings.TrimSpace(r.FormValue("name")), URL: strings.TrimSpace(r.FormValue("url")),
		Interval: strings.TrimSpace(r.FormValue("interval")), Timeout: strings.TrimSpace(r.FormValue("timeout")),
		ExpectStatus: strings.TrimSpace(r.FormValue("expect_status")), Keyword: r.FormValue("keyword")}
}

// toTarget validates the form and returns a Target (without ID).
func (f targetForm) toTarget(defaults *config.Config) (store.Target, error) {
	t := store.Target{Name: f.Name, URL: f.URL, Keyword: f.Keyword,
		Interval: defaults.Interval.Duration, Timeout: defaults.Timeout.Duration}
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
	if err := config.ValidateTarget(t.Name, t.URL, t.Interval, t.Timeout, t.ExpectStatus); err != nil {
		return t, err
	}
	return t, nil
}

type targetsData struct {
	base
	Targets []targetView
	Form    targetForm
}

func (s *Server) targetsPage(w http.ResponseWriter, r *http.Request) {
	d := targetsData{base: s.base(r, "targets", "Targets"),
		Form: targetForm{Action: "/targets", Submit: "Add target", Interval: s.cfg.Interval.String(), Timeout: s.cfg.Timeout.String()}}
	var err error
	if d.Targets, err = s.views(r.Context(), false); err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, "targets", "layout", d)
}

func (s *Server) targetCreate(w http.ResponseWriter, r *http.Request) {
	f := formFromRequest(r, "/targets", "Add target")
	t, err := f.toTarget(s.cfg)
	if err == nil {
		t, err = s.store.CreateTarget(r.Context(), t, s.now())
	}
	if err != nil {
		d := targetsData{base: s.base(r, "targets", "Targets"), Form: f}
		d.Error = err.Error()
		d.Targets, _ = s.views(r.Context(), false)
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "targets", "layout", d)
		return
	}
	if err := s.mon.Reload(r.Context(), t); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/targets/%d", t.ID), "Target added.")
}

type targetData struct {
	base
	View      targetView
	Form      targetForm
	Range     string
	Ranges    []string
	Chart     template.HTML
	Incidents []store.Incident
	Checks    []store.Check
}

var ranges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

func (s *Server) targetData(r *http.Request, t store.Target, st monitor.State) (targetData, error) {
	ctx := r.Context()
	d := targetData{base: s.base(r, "targets", t.Name), Form: formFromTarget(t), Ranges: []string{"24h", "7d", "30d"}}
	d.Range = r.URL.Query().Get("range")
	span, ok := ranges[d.Range]
	if !ok {
		d.Range, span = "24h", 24*time.Hour
	}
	var err error
	if d.View, err = s.view(ctx, st, false); err != nil {
		return d, err
	}
	now := s.now()
	checks, err := s.store.ChecksSince(ctx, t.ID, now.Add(-span))
	if err != nil {
		return d, err
	}
	if d.Incidents, err = s.store.ListIncidents(ctx, store.IncidentFilter{TargetID: t.ID, Limit: 50}); err != nil {
		return d, err
	}
	d.Chart = latencySVG(checks, d.Incidents, now.Add(-span), now)
	if d.Checks, err = s.store.RecentChecks(ctx, t.ID, 30); err != nil {
		return d, err
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
	s.render(w, "target", "layout", d)
}

func (s *Server) targetUpdate(w http.ResponseWriter, r *http.Request) {
	t, st, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	f := formFromRequest(r, fmt.Sprintf("/targets/%d", t.ID), "Save changes")
	nt, err := f.toTarget(s.cfg)
	if err == nil {
		nt.ID, nt.Paused, nt.CreatedAt = t.ID, t.Paused, t.CreatedAt
		err = s.store.UpdateTarget(r.Context(), nt)
	}
	if err != nil {
		d, derr := s.targetData(r, t, st)
		if derr != nil {
			s.fail(w, derr)
			return
		}
		d.Form, d.Error = f, err.Error()
		w.WriteHeader(http.StatusUnprocessableEntity)
		s.render(w, "target", "layout", d)
		return
	}
	if err := s.mon.Reload(r.Context(), nt); err != nil {
		s.fail(w, err)
		return
	}
	redirect(w, r, fmt.Sprintf("/targets/%d", t.ID), "Target saved.")
}

func (s *Server) targetDelete(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	s.mon.Remove(t.ID)
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

func (s *Server) targetCheck(w http.ResponseWriter, r *http.Request) {
	t, _, ok := s.loadTarget(w, r)
	if !ok {
		return
	}
	if t.Paused {
		redirect(w, r, fmt.Sprintf("/targets/%d", t.ID), "Target is paused.")
		return
	}
	s.mon.CheckAsync(t.ID)
	redirect(w, r, fmt.Sprintf("/targets/%d", t.ID), "Check started for "+t.Name+". Refresh in a moment.")
}

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
	s.render(w, "incidents", "layout", d)
}

type alertsData struct {
	base
	N         config.Notify
	To        string
	Has       map[string]bool
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
	n := s.notifyConfig(r.Context())
	s.renderAlerts(w, r, n, "")
}

func (s *Server) renderAlerts(w http.ResponseWriter, r *http.Request, n config.Notify, errMsg string) {
	d := alertsData{base: s.base(r, "alerts", "Alerts"), N: n, Has: map[string]bool{}, Threshold: s.cfg.FailureThreshold}
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
	s.render(w, "alerts", "layout", d)
}

// parseNotifyForm builds notify settings from the alerts form.
func parseNotifyForm(r *http.Request) config.Notify {
	v := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	var n config.Notify
	if v("tg_token") != "" || v("tg_chat") != "" {
		n.Telegram = &config.Telegram{BotToken: v("tg_token"), ChatID: v("tg_chat")}
	}
	if v("slack_url") != "" {
		n.Slack = &config.Slack{WebhookURL: v("slack_url")}
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
			Password: v("smtp_pass"), From: v("smtp_from"), To: to}
	}
	return n
}

// saveNotify validates and persists settings, swapping the live notifiers.
// It renders the form with an error and returns false on invalid input.
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
	redirect(w, r, "/alerts", "Alert settings saved.")
}

// alertsTest saves the form first, then sends a test through one channel,
// so what you see in the form is what gets tested.
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
	s.render(w, "login", "layout", loginData{base: s.base(r, "login", "Log in"), Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := loginData{base: s.base(r, "login", "Log in"), Next: safeNext(r.FormValue("next"))}
	key := clientKey(r, s.cfg.TrustProxy)
	if s.limiter.blocked(key) {
		d.Error = "Too many failed attempts. Wait a minute and try again."
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, "login", "layout", d)
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
	s.render(w, "login", "layout", d)
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
	Status      string     `json:"status"`
	Since       *time.Time `json:"since,omitempty"`
	LastChecked *time.Time `json:"last_checked,omitempty"`
	LatencyMS   *int64     `json:"latency_ms,omitempty"`
	StatusCode  int        `json:"status_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	Uptime24h   float64    `json:"uptime_24h"`
	Uptime7d    float64    `json:"uptime_7d"`
	Paused      bool       `json:"paused"`
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	views, err := s.views(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := make([]apiTarget, 0, len(views))
	for _, v := range views {
		st := v.State
		a := apiTarget{ID: st.Target.ID, Name: st.Target.Name, URL: st.Target.URL, Status: st.Status.String(),
			Uptime24h: round2(v.Up24.UptimePercent()), Uptime7d: round2(v.Up7.UptimePercent()), Paused: st.Target.Paused}
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
		ID         int64      `json:"id"`
		Target     string     `json:"target"`
		TargetID   int64      `json:"target_id"`
		StartedAt  time.Time  `json:"started_at"`
		EndedAt    *time.Time `json:"ended_at,omitempty"`
		DurationS  int64      `json:"duration_s"`
		Cause      string     `json:"cause"`
		AlertedVia string     `json:"alerted_via"`
	}
	now := s.now()
	out := make([]in, 0, len(list))
	for _, i := range list {
		out = append(out, in{i.ID, i.TargetName, i.TargetID, i.StartedAt, i.EndedAt, int64(i.Duration(now).Seconds()), i.Cause, i.Channels})
	}
	writeJSON(w, map[string]any{"incidents": out})
}

// ---- helpers ----

func (s *Server) render(w http.ResponseWriter, page, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("render", "page", page, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
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

// backTo returns the Referer path when it is a local page, else fallback.
func backTo(r *http.Request, fallback string) string {
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host && strings.HasPrefix(ref.Path, "/") {
		return ref.Path
	}
	return fallback
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

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
