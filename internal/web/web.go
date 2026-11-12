// Package web serves the dashboard, admin pages, public status page and JSON API.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/password"
	"uptimemon/internal/ratelimit"
	"uptimemon/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server holds the HTTP handlers.
type Server struct {
	cfg           *config.Config
	sys           *store.Store
	mon           *monitor.Monitor
	logger        *slog.Logger
	now           func() time.Time
	hasher        *password.Hasher
	setupMode     atomic.Bool
	upgraded      bool
	afterSetup    func(ctx context.Context) error
	loginByAddr   *ratelimit.Limiter
	loginByPair   *ratelimit.Limiter
	loginByEmail  *ratelimit.Limiter
	loginSlow     *ratelimit.Limiter
	deviceKey     []byte // HMAC key for device cookies, fresh per process
	statusLimiter *ratelimit.Limiter
	apiLimiter    *ratelimit.Limiter // JSON API, per session

	streamsMu     sync.Mutex
	streams       map[int64]int // open /events streams per user
	setupLimiter  *ratelimit.Limiter
	signupByAddr  *ratelimit.Limiter // sign-ups per client address
	signupGlobal  *ratelimit.Limiter // sign-ups across the instance, key "all"
	mail          Mailer
	mailByAddr    *ratelimit.Limiter // forgot and resend, per client address
	dupNotice     *ratelimit.Limiter // duplicate sign-up notices, per user ID
	passwordCheck *ratelimit.Limiter // current-password checks, per user
	createLimiter *ratelimit.Limiter // monitor creations, per workspace
	checkTarget   *ratelimit.Limiter // Check now, per target
	checkWS       *ratelimit.Limiter // Check now, per workspace
	testURL       *ratelimit.Limiter // Test URL, per workspace
	alertTest     *ratelimit.Limiter // alert test sends, per workspace
	pages         map[string]*template.Template
	mux           *http.ServeMux

	statusCache pageCache

	adminMu sync.Mutex // serialises the panel's disable, enable and delete
}

// Mailer queues system email. Send charges the budgets first and calls
// compose only when the message will be sent (see internal/mail).
type Mailer interface {
	Send(to string, compose func() (subject, body string, err error)) error
	Notice(to, subject, body string)
}

// Options configure a Server.
type Options struct {
	Config  *config.Config
	Store   *store.Store
	Monitor *monitor.Monitor
	Logger  *slog.Logger     // nil means slog.Default(), as in monitor.New
	Now     func() time.Time // nil means time.Now

	Mail       Mailer           // nil means no system mail
	Hasher     *password.Hasher // nil means password.Default()
	SetupMode  bool             // no account exists yet; only the setup flow is served
	Upgraded   bool             // the database predates accounts
	AfterSetup func(ctx context.Context) error
}

// New builds a Server. Config, Store and Monitor are required.
func New(o Options) (*Server, error) {
	if o.Config == nil || o.Store == nil || o.Monitor == nil {
		return nil, errors.New("web: Options.Config, Store and Monitor are required")
	}
	cfg, st, mon, logger, now := o.Config, o.Store, o.Monitor, o.Logger, o.Now
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	if cfg.Signup == "open" && o.Mail == nil {
		return nil, errors.New(`web: signup "open" needs Options.Mail`)
	}
	s := &Server{cfg: cfg, sys: st, mon: mon, logger: logger, now: now, mail: o.Mail,
		hasher: o.Hasher, upgraded: o.Upgraded, afterSetup: o.AfterSetup,
		pages: map[string]*template.Template{}}
	if s.hasher == nil {
		s.hasher = password.Default()
	}
	s.setupMode.Store(o.SetupMode)
	s.loginByAddr = ratelimit.New(loginAddrWindow, loginAddrMax, 0)
	s.loginByPair = ratelimit.New(loginPairWindow, loginPairMax, 0)
	s.loginByEmail = ratelimit.New(loginEmailWindow, loginEmailMax, 0)
	s.loginSlow = ratelimit.New(loginSlowWindow, loginSlowMax, 0)
	for _, l := range []*ratelimit.Limiter{s.loginByAddr, s.loginByPair, s.loginByEmail, s.loginSlow} {
		l.Now = now
	}
	s.deviceKey = make([]byte, 32)
	if _, err := rand.Read(s.deviceKey); err != nil {
		return nil, fmt.Errorf("web: device key: %w", err)
	}
	s.setupLimiter = ratelimit.New(setupWindow, setupMaxAttempts, 0)
	s.setupLimiter.Now = now
	s.signupByAddr = ratelimit.New(time.Hour, signupPerAddrHour, 0)
	s.signupByAddr.Now = now
	s.signupGlobal = ratelimit.New(time.Hour, *cfg.Limits.SignupsPerHour, 0)
	s.signupGlobal.Now = now
	s.mailByAddr = ratelimit.New(time.Hour, 10, 0)
	s.mailByAddr.Now = now
	s.dupNotice = ratelimit.New(24*time.Hour, 1, 0)
	s.dupNotice.Now = now
	s.passwordCheck = ratelimit.New(passwordCheckWindow, passwordCheckMax, 0)
	s.passwordCheck.Now = now
	s.createLimiter = ratelimit.New(time.Hour, 30, 0)
	s.checkTarget = ratelimit.New(10*time.Second, 1, 0)
	s.checkWS = ratelimit.New(time.Minute, 30, 0)
	s.testURL = ratelimit.New(time.Minute, 10, 0)
	s.alertTest = ratelimit.New(time.Minute, 5, 0)
	for _, l := range []*ratelimit.Limiter{s.createLimiter, s.checkTarget, s.checkWS, s.testURL, s.alertTest} {
		l.Now = now
	}
	s.statusLimiter = ratelimit.New(time.Minute, statusRatePerMinute, 0)
	s.statusLimiter.Now = now
	s.apiLimiter = ratelimit.New(time.Minute, apiRatePerMinute, 0)
	s.apiLimiter.Now = now
	s.statusCache.maxBytes = statusCacheMaxBytes
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
		// data-sort keys for the data tables: unix ms, duration ms, numeric percent; "" sorts last
		"ums": func(v any) string {
			switch x := v.(type) {
			case time.Time:
				if !x.IsZero() {
					return strconv.FormatInt(x.UnixMilli(), 10)
				}
			case *time.Time:
				if x != nil && !x.IsZero() {
					return strconv.FormatInt(x.UnixMilli(), 10)
				}
			}
			return ""
		},
		"dms": func(d time.Duration) int64 { return d.Milliseconds() },
		"pctv": func(st store.Stats) string {
			if st.Total == 0 {
				return ""
			}
			return strconv.FormatFloat(round2(st.UptimePercent()), 'f', -1, 64)
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
	for _, page := range []string{"overview", "monitors", "target", "incidents", "alerts", "maintenance", "account", "admin_users", "admin_delete", "admin_invites"} {
		files := append([]string{"templates/layout.html"}, shared...)
		files = append(files, "templates/"+page+".html")
		t, err := template.New("layout.html").Funcs(funcs).ParseFS(templateFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", page, err)
		}
		s.pages[page] = t
	}
	for _, page := range []string{"status", "login", "setup", "reset", "signup", "verify", "forgot"} {
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
func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.setupGate(s.withAuthCache(s.mux)))
}

// healthz reports ok only when the database answers, so an orchestrator
// restarts the process if storage is wedged.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.sys.Ping(ctx); err != nil {
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
	Title      string
	Page       string
	Admin      bool
	UserEmail  string
	Flash      string
	Error      string
	Now        time.Time
	Theme      string
	Unacked    int
	DownCount  int
	NewForm    targetForm
	Groups     []string
	Maint      []store.Maintenance // windows active right now
	StatusURL  string              // /s/<slug> of this workspace
	IsOperator bool                // show the Admin sidebar link
	NoRefresh  bool                // live refresh must not replace #content
}

// base builds the view model of a signed-in workspace page.
func (s *Server) base(r *http.Request, a *authCtx, page, title string) base {
	b := s.baseFor(r.Context(), a.WS, page, title, themeFrom(r), true)
	b.UserEmail = a.User.Email
	b.IsOperator = a.User.IsOperator
	// Only signed-in flows redirect with a flash; never echo ?msg= to anonymous
	// visitors, and only show messages the server itself signed.
	b.Flash = flashFrom(r)
	return b
}

// page builds the view model of a page that has no workspace (sign-in,
// setup, reset).
func (s *Server) page(r *http.Request, title string) base {
	return base{Title: title, Now: s.now(), Theme: themeFrom(r)}
}

// baseFor is base without a request, so the anonymous status page can be
// built (and cached) from nothing but the workspace.
func (s *Server) baseFor(ctx context.Context, ws *store.WS, page, title, theme string, admin bool) base {
	b := base{Title: title, Page: page, Admin: admin, Now: s.now(), Theme: theme}
	b.StatusURL = s.statusURL(ctx, ws)
	b.NewForm = targetForm{Action: "/targets", Submit: "Create monitor", ShowTest: true,
		Interval: shortDur(s.cfg.Interval.Duration), Timeout: shortDur(s.cfg.Timeout.Duration)}
	if b.Admin {
		b.Unacked, _ = ws.CountUnacknowledged(ctx)
	}
	b.Maint, _ = ws.ActiveMaintenance(ctx, b.Now)
	for _, st := range s.mon.Snapshot(ws.ID()) {
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

func (s *Server) view(ctx context.Context, ws *store.WS, st monitor.State, o viewOpts) (targetView, error) {
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
	if m, in, err := ws.InMaintenance(ctx, st.Target.Group, now); err != nil {
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
	if v.Up24, err = s.uptime(ctx, ws, st.Target.ID, now, 1); err != nil {
		return v, err
	}
	if v.Up7, err = s.uptime(ctx, ws, st.Target.ID, now, 7); err != nil {
		return v, err
	}
	if v.Up30, err = s.uptime(ctx, ws, st.Target.ID, now, 30); err != nil {
		return v, err
	}
	if o.spark {
		checks, err := ws.ChecksSince(ctx, st.Target.ID, now.Add(-30*time.Minute))
		if err != nil {
			return v, err
		}
		v.Spark = sparkSVG(checks, now.Add(-30*time.Minute), now)
	}
	if o.days {
		days, err := ws.DailyUptime(ctx, st.Target.ID, now, 90)
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
func (s *Server) uptime(ctx context.Context, ws *store.WS, targetID int64, now time.Time, days int) (store.Stats, error) {
	span := time.Duration(days) * 24 * time.Hour
	if span <= s.cfg.Retention.Duration {
		return ws.Stats(ctx, targetID, now.Add(-span))
	}
	return ws.RollupStats(ctx, targetID, now.UTC().AddDate(0, 0, -(days-1)))
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

func (s *Server) views(ctx context.Context, ws *store.WS, o viewOpts) ([]targetView, error) {
	states := s.mon.Snapshot(ws.ID())
	out := make([]targetView, 0, len(states))
	for _, st := range states {
		v, err := s.view(ctx, ws, st, o)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
