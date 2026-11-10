// Package web serves the dashboard, admin pages, public status page and JSON API.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/ratelimit"
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
	limiter  *ratelimit.Limiter
	pages    map[string]*template.Template
	mux      *http.ServeMux

	statusCache pageCache
}

// New builds a Server. now may be nil.
func New(cfg *config.Config, st *store.Store, mon *monitor.Monitor, logger *slog.Logger, now func() time.Time) (*Server, error) {
	if now == nil {
		now = time.Now
	}
	s := &Server{cfg: cfg, store: st, mon: mon, logger: logger, now: now,
		sessions: newSessions(now), pages: map[string]*template.Template{}}
	s.limiter = ratelimit.New(time.Minute, loginMaxFailures, 0)
	s.limiter.Now = now
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
		"asset": assetURL,
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
	}
	shared := []string{"templates/icons.html", "templates/form.html", "templates/partials.html"}
	for _, page := range []string{"overview", "monitors", "target", "incidents", "alerts", "maintenance"} {
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
func (s *Server) Handler() http.Handler { return s.securityHeaders(s.mux) }

func (s *Server) routes() {
	m := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	m.Handle("GET /static/", http.StripPrefix("/static/", cacheControl(http.FileServer(http.FS(static)))))
	m.HandleFunc("GET /healthz", s.healthz)
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
	m.HandleFunc("GET /maintenance", admin(s.maintenancePage))
	m.HandleFunc("POST /maintenance", post(s.maintenanceCreate))
	m.HandleFunc("POST /maintenance/{id}/end", post(s.maintenanceEnd))
	m.HandleFunc("POST /maintenance/{id}/delete", post(s.maintenanceDelete))
	m.HandleFunc("GET /api/maintenance", admin(s.apiMaintenance))
	m.HandleFunc("GET /api/status", admin(s.apiStatus))
	m.HandleFunc("GET /api/targets/{id}/checks", admin(s.apiChecks))
	m.HandleFunc("GET /api/incidents", admin(s.apiIncidents))
	s.mux = m
}

// healthz reports ok only when the database answers, so an orchestrator
// restarts the process if storage is wedged.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		s.logger.Error("health check", "err", err)
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, "ok")
}

// assetVersions maps each embedded static file to a short hash of its
// content. Templates link assets with ?v=<hash>, so browsers fetch new
// JS/CSS right after an upgrade instead of running stale code against new
// markup, and versioned URLs can be cached for a long time.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	entries, _ := fs.ReadDir(staticFS, "static")
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:6])
	}
	return out
}()

func assetURL(name string) string {
	return "/static/" + name + "?v=" + assetVersions[name]
}

func cacheControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("v"); v != "" && v == assetVersions[strings.TrimPrefix(r.URL.Path, "/")] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
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
	Maint       []store.Maintenance // windows active right now
}

func (s *Server) base(r *http.Request, page, title string) base {
	b := base{Title: title, Page: page, Admin: s.isAdmin(r), AuthEnabled: s.authEnabled(), Now: s.now(), Theme: themeFrom(r)}
	if b.Admin {
		// Only admin flows redirect with a flash; never echo ?msg= to anonymous
		// visitors, and only show messages the server itself signed.
		b.Flash = flashFrom(r)
	}
	b.NewForm = targetForm{Action: "/targets", Submit: "Create monitor", ShowTest: true,
		Interval: shortDur(s.cfg.Interval.Duration), Timeout: shortDur(s.cfg.Timeout.Duration)}
	if b.Admin {
		b.Unacked, _ = s.store.CountUnacknowledged(r.Context())
	}
	b.Maint, _ = s.store.ActiveMaintenance(r.Context(), b.Now)
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
	Maint     *store.Maintenance // the window covering this target now, if any
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
	if m, in, err := s.store.InMaintenance(ctx, st.Target.Group, now); err != nil {
		return v, err
	} else if in {
		v.Maint = &m
		// An incident that began before the window stays visible as down and
		// a paused monitor stays paused; anything else reads as planned maintenance.
		if st.Status != monitor.Down && st.Status != monitor.Paused {
			v.Class, v.Label = "maint", "Maintenance"
		}
	}
	var err error
	if v.Up24, err = s.uptime(ctx, st.Target.ID, now, 1); err != nil {
		return v, err
	}
	if v.Up7, err = s.uptime(ctx, st.Target.ID, now, 7); err != nil {
		return v, err
	}
	if v.Up30, err = s.uptime(ctx, st.Target.ID, now, 30); err != nil {
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

// uptime returns stats for the last days days. Raw checks are exact but only
// kept for the retention period; longer windows use the daily rollups,
// covering this day and the days-1 before it in UTC.
func (s *Server) uptime(ctx context.Context, targetID int64, now time.Time, days int) (store.Stats, error) {
	span := time.Duration(days) * 24 * time.Hour
	if span <= s.cfg.Retention.Duration {
		return s.store.Stats(ctx, targetID, now.Add(-span))
	}
	return s.store.RollupStats(ctx, targetID, now.UTC().AddDate(0, 0, -(days-1)))
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
