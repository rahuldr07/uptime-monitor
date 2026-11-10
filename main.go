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
	"syscall"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/monitor"
	"uptimemon/internal/notify"
	"uptimemon/internal/store"
	"uptimemon/internal/web"
)

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
	if !setup && cfg.AdminPassword != "" {
		logger.Warn("admin_password is ignored once an account exists")
	}
	// seedOperator puts the config file's targets and alert settings into the
	// operator's workspace (workspace 1).
	seedOperator := func() error {
		ws1 := st.Workspace(1)
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

	mon := monitor.New(monitor.Options{Store: st, Logger: logger, FailureThreshold: cfg.FailureThreshold,
		Notifiers: func(ctx context.Context, ws int64) (notify.Multi, error) {
			n, err := st.Workspace(ws).Notify(ctx)
			if err != nil {
				return nil, err
			}
			return notify.FromConfig(n, logger, cfg.AllowPrivate(ws == 1)), nil
		},
		PrivateAllowed: func(ws int64) bool { return cfg.AllowPrivate(ws == 1) }})
	if err := mon.Start(ctx); err != nil {
		return err
	}
	srv, err := web.New(web.Options{Config: cfg, Store: st, Monitor: mon, Logger: logger,
		SetupMode: setup, Upgraded: upgraded,
		AfterSetup: func(ctx context.Context) error {
			if err := seedOperator(); err != nil {
				return err
			}
			ts, err := st.Workspace(1).ListTargets(ctx)
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
	go pruneLoop(ctx, st, cfg.Retention.Duration, logger)

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

func pruneLoop(ctx context.Context, st *store.Store, retention time.Duration, logger *slog.Logger) {
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
