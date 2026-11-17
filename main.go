// Command uptime-monitor checks URLs on a schedule, alerts on state changes
// and serves a dashboard and public status page.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/config"
	"github.com/rahuldr07/uptime-monitor/internal/mail"
	"github.com/rahuldr07/uptime-monitor/internal/monitor"
	"github.com/rahuldr07/uptime-monitor/internal/netguard"
	"github.com/rahuldr07/uptime-monitor/internal/notify"
	"github.com/rahuldr07/uptime-monitor/internal/oauth"
	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/web"
)

// userAgent identifies this instance on probes, with its public address when
// base_url is set so a probed site's owner can tell who is checking them.
func userAgent(version, baseURL string) string {
	ua := "uptime-monitor/" + version
	if baseURL != "" {
		ua += " (+" + baseURL + ")"
	}
	return ua
}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// rollupRetention is how long daily uptime rollups are kept. It must cover
// the 90-day status page history.
const rollupRetention = 400 * 24 * time.Hour

// configCandidates are tried in order when -config is not given: the
// working directory first (in the container that is /data, the original
// layout), then the location the container image uses, then the one the
// systemd unit uses.
var configCandidates = []string{"config.json", "/etc/uptime-monitor/config.json", "/etc/uptime-monitor.json"}

// resolveConfig returns the explicit -config value, or the first candidate
// that exists, or the first candidate (so the error names a sensible path).
func resolveConfig(explicit string, candidates []string) string {
	if explicit != "" {
		return explicit
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return candidates[0]
}

func main() {
	cfgFlag := flag.String("config", "", "path to config file (default: first of ./config.json, /etc/uptime-monitor/config.json, /etc/uptime-monitor.json)")
	logFormat := flag.String("log-format", "text", "log format: text or json")
	showVersion := flag.Bool("version", false, "print the version and exit")
	backupPath := flag.String("backup", "", "write a consistent copy of the database to this path and exit")
	setupLink := flag.Bool("setup-link", false, "print a one-time first-run setup link and exit")
	resetEmail := flag.String("reset-password", "", "print a one-time password reset link for this email and exit")
	invite := flag.Bool("invite", false, "print a one-time sign-up link and exit; an email address, if any, must come last and binds the invitation to it")
	enableEmail := flag.String("enable-user", "", "re-enable a disabled account by email and exit (restart the server afterwards so its monitors resume)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	cfgPath := resolveConfig(*cfgFlag, configCandidates)
	if *backupPath != "" {
		src, err := backup(cfgPath, *backupPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("backup of %s written to %s\n", src, *backupPath)
		return
	}
	if *setupLink {
		link, err := newSetupLink(cfgPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(link)
		return
	}
	if *enableEmail != "" {
		if err := enableUser(cfgPath, *enableEmail); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("enabled %s\n", *enableEmail)
		return
	}
	if *invite {
		email, err := inviteEmailArg(flag.Args())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		link, err := newInviteLink(cfgPath, email)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(link)
		return
	}
	if *resetEmail != "" {
		link, err := newResetLink(cfgPath, *resetEmail)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(link)
		return
	}
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, opts)
	switch *logFormat {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
	default:
		fmt.Fprintf(os.Stderr, "unknown -log-format %q (use text or json)\n", *logFormat)
		os.Exit(2)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger) // packages without an injected logger (store migrations) log the same way
	if err := run(cfgPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath string, logger *slog.Logger) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.InsecureNoAuth {
		logger.Warn("insecure_no_auth has no effect; sign-in is always required")
	}
	if cfg.AllowPrivateTargets == "all" {
		logger.Warn(`allow_private_targets is "all": any non-operator account, including one made from an outstanding invite, can reach internal addresses`)
	}
	if cfg.TrustProxy && !cfg.HTTPSBase() {
		logger.Warn("trust_proxy is set without an https base_url: set base_url to your https address so session cookies are always Secure")
	}
	if cfg.Mail != nil {
		logger.Info("system mail on", "smtp_host", cfg.Mail.SMTPHost)
	} else {
		logger.Info("system mail off")
	}
	logger.Info("starting", "version", version, "config", cfgPath, "listen", cfg.Listen, "db", cfg.DB)
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	nUsers, err := st.CountUsers(ctx)
	if err != nil {
		return err
	}
	setup := nUsers == 0
	upgraded, err := st.IsUpgradedInstall(ctx)
	if err != nil {
		return err
	}
	if setup {
		raw, err := st.CreateToken(ctx, store.TokenSetup, 0, setupTokenTTL, time.Now())
		if err != nil {
			return err
		}
		logger.Warn("setup required: open " + cfg.LinkBase() + "/setup?token=" + raw)
	}
	if !setup && cfg.AdminPassword != "" {
		logger.Warn("admin_password is ignored once an account exists")
	}
	// seedOperator puts the config file's targets and alert settings into the
	// operator's workspace.
	seedOperator := func() error {
		ws1 := st.Workspace(store.OperatorWorkspaceID)
		if err := seedTargets(ctx, ws1, cfg, logger); err != nil {
			return err
		}
		return seedNotify(ctx, ws1, cfg)
	}
	if !setup {
		if err := seedOperator(); err != nil {
			return err
		}
	}

	mailer := newMailer(ctx, cfg, logger)
	sysMail, _ := mailer.(notify.SystemMail) // nil without system mail
	mon := monitor.New(monitor.Options{Store: st, Logger: logger, FailureThreshold: cfg.FailureThreshold,
		Notifiers: func(ctx context.Context, ws int64) (notify.Multi, error) {
			n, err := st.Workspace(ws).Notify(ctx)
			if err != nil {
				return nil, err
			}
			restricted := ws != store.OperatorWorkspaceID
			m := notify.FromConfig(n, logger, cfg.AllowPrivate(!restricted), restricted)
			return notify.WithServerEmail(m, n, sysMail, ws, st.Workspace(ws).MemberEmails), nil
		},
		PrivateAllowed: func(ws int64) bool { return cfg.AllowPrivate(ws == store.OperatorWorkspaceID) },
		UserAgent:      userAgent(version, cfg.BaseURL)})
	if err := mon.Start(ctx); err != nil {
		return err
	}
	srv, err := web.New(web.Options{Config: cfg, Store: st, Monitor: mon, Logger: logger,
		Mail: mailer, AlertMail: sysMail,
		OAuth:     newOAuth(cfg),
		SetupMode: setup, Upgraded: upgraded,
		AfterSetup: func(ctx context.Context) error {
			if err := seedOperator(); err != nil {
				return err
			}
			ts, err := st.Workspace(store.OperatorWorkspaceID).ListTargets(ctx)
			if err != nil {
				return err
			}
			for _, t := range ts {
				if err := mon.Reload(ctx, t.ID); err != nil {
					return err
				}
			}
			return nil
		}})
	if err != nil {
		return err
	}

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn)}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go pruneLoop(ctx, st, mon, cfg.Retention.Duration, logger)

	select {
	case <-ctx.Done():
		logger.Info("shutting down (send the signal again to force exit)")
	case err := <-errCh:
		stop()
		return err
	}
	stop() // restore default signal handling so a second Ctrl-C exits immediately
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http shutdown", "err", err)
	}
	done := make(chan struct{})
	go func() { mon.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		logger.Warn("alerts still in flight at shutdown deadline; exiting")
	}
	return nil
}

// setupTokenTTL is how long a setup link stays valid.
const setupTokenTTL = 24 * time.Hour

// newSetupLink creates a setup token and returns the link that uses it. It
// fails when setup is already complete.
func newSetupLink(cfgPath string) (string, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return "", err
	}
	defer st.Close()
	ctx := context.Background()
	n, err := st.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", errors.New("setup is already complete")
	}
	raw, err := st.CreateToken(ctx, store.TokenSetup, 0, setupTokenTTL, time.Now())
	if err != nil {
		return "", err
	}
	return cfg.LinkBase() + "/setup?token=" + raw, nil
}

// resetTokenTTL is how long a reset link from -reset-password works.
const resetTokenTTL = time.Hour

// newResetLink creates a reset token for the user with this email and returns
// the link that uses it.
func newResetLink(cfgPath, email string) (string, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return "", err
	}
	defer st.Close()
	ctx := context.Background()
	u, _, err := st.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return "", errors.New("no such user")
	}
	if err != nil {
		return "", err
	}
	if u.DisabledAt != nil {
		return "", errors.New("user is disabled")
	}
	raw, err := st.CreateToken(ctx, store.TokenReset, u.ID, resetTokenTTL, time.Now())
	if err != nil {
		return "", err
	}
	return cfg.LinkBase() + "/reset?token=" + raw, nil
}

// enableUser re-enables the account with this email. It also works for
// operators: this is the recovery path. The running server must restart before
// the user's monitors resume.
func enableUser(cfgPath, email string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	u, _, err := st.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("no such user")
	}
	if err != nil {
		return err
	}
	_, err = st.SetUserDisabled(ctx, u.ID, false, time.Now())
	return err
}

// inviteEmailArg returns the optional email after -invite. Anything after
// the flag that is not a single plain argument is a usage error, so a
// misplaced flag is never taken for an address.
func inviteEmailArg(args []string) (string, error) {
	const usage = "usage: -invite [email] (the email, if any, must come last, after all other flags)"
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		return "", errors.New(usage)
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return "", nil
}

// inviteTTL is how long a link from -invite works.
const inviteTTL = 7 * 24 * time.Hour

// newInviteLink creates an invitation, optionally bound to email, and returns
// the sign-up link that uses it. It works in every sign-up mode, but not
// before the first account exists.
func newInviteLink(cfgPath, email string) (string, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", err
	}
	st, err := store.Open(cfg.DB)
	if err != nil {
		return "", err
	}
	defer st.Close()
	ctx := context.Background()
	n, err := st.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	if n == 0 {
		return "", errors.New("finish setup first")
	}
	raw, err := st.CreateInvite(ctx, email, inviteTTL, time.Now())
	if err != nil {
		return "", err
	}
	return cfg.LinkBase() + "/signup?invite=" + raw, nil
}

// backup copies the configured database to dest and returns the absolute
// source path, so a relative "db" resolved from the wrong directory is visible.
func backup(cfgPath, dest string) (string, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "", err
	}
	src, err := filepath.Abs(cfg.DB)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	return src, store.Backup(ctx, src, dest)
}

// seedTargets inserts config-file targets that do not exist yet. Targets
// edited or added in the UI are left alone.
func seedTargets(ctx context.Context, ws *store.WS, cfg *config.Config, logger *slog.Logger) error {
	for _, t := range cfg.Targets {
		if _, err := ws.GetTargetByName(ctx, t.Name); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := ws.CreateTarget(ctx, store.Target{Name: t.Name, URL: t.URL, Interval: t.Interval.Duration,
			Timeout: t.Timeout.Duration, ExpectStatus: t.ExpectStatus, Keyword: t.Keyword, Group: t.Group}, time.Now()); err != nil {
			return fmt.Errorf("seed target %q: %w", t.Name, err)
		}
		logger.Info("added target from config", "name", t.Name)
	}
	return nil
}

// seedNotify stores the config file's notify settings when the workspace has
// none, so the alerts page shows them. Settings saved from the UI win.
func seedNotify(ctx context.Context, ws *store.WS, cfg *config.Config) error {
	if _, err := ws.GetSetting(ctx, "notify"); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	b, err := json.Marshal(cfg.Notify)
	if err != nil {
		return nil
	}
	return ws.SetSetting(ctx, "notify", string(b))
}

// newMailer returns the system mail queue and starts its worker, or a literal
// nil Mailer when no mail is configured. The host is the operator's choice,
// so private addresses are allowed (D-126).
func newMailer(ctx context.Context, cfg *config.Config, logger *slog.Logger) web.Mailer {
	if cfg.Mail == nil {
		return nil
	}
	m := cfg.Mail
	sender := notify.Email{Host: m.SMTPHost, Port: m.SMTPPort, Username: m.Username, Password: m.Password,
		From: m.From, RequireTLS: *m.RequireTLS, Dial: netguard.Dialer(true, 30*time.Second).DialContext}
	q := mail.New(mail.Options{Sender: sender, PerHour: *cfg.Limits.MailPerHour, PerDay: *cfg.Limits.MailPerDay, Logger: logger})
	go q.Run(ctx)
	return q
}

// newOAuth builds the enabled sign-in providers, or nil when there are none.
func newOAuth(cfg *config.Config) []*oauth.Provider {
	if cfg.OAuth == nil {
		return nil
	}
	var out []*oauth.Provider
	if g := cfg.OAuth.Google; g != nil {
		out = append(out, oauth.Google(g.ClientID, g.ClientSecret))
	}
	if g := cfg.OAuth.GitHub; g != nil {
		out = append(out, oauth.GitHub(g.ClientID, g.ClientSecret))
	}
	return out
}

// unverifiedMaxAge is how long an account may stay unverified.
const unverifiedMaxAge = 48 * time.Hour

func pruneLoop(ctx context.Context, st *store.Store, mon *monitor.Monitor, retention time.Duration, logger *slog.Logger) {
	prune := func() {
		n, err := st.Prune(ctx, time.Now().Add(-retention))
		if err != nil && ctx.Err() == nil {
			logger.Error("prune", "err", err)
		} else if n > 0 {
			logger.Info("pruned old checks", "rows", n)
		}
		if _, err := st.PruneRollups(ctx, time.Now().Add(-rollupRetention)); err != nil && ctx.Err() == nil {
			logger.Error("prune rollups", "err", err)
		}
		if _, err := st.PruneSessions(ctx, time.Now()); err != nil && ctx.Err() == nil {
			logger.Error("prune sessions", "err", err)
		}
		if _, err := st.PruneTokens(ctx, time.Now()); err != nil && ctx.Err() == nil {
			logger.Error("prune tokens", "err", err)
		}
		ids, err := st.PruneUnverified(ctx, time.Now().Add(-unverifiedMaxAge))
		for _, id := range ids {
			mon.ForgetWorkspace(id)
		}
		if err != nil && ctx.Err() == nil {
			logger.Error("prune unverified", "err", err)
		}
		if len(ids) > 0 {
			logger.Info("pruned unverified accounts", "event", "account_pruned", "count", len(ids))
		}
	}
	prune()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}
