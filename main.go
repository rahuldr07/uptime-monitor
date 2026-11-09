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
	if err := cfg.CheckAuth(); err != nil {
		return err
	}
	logger.Info("starting", "version", version, "config", cfgPath, "listen", cfg.Listen, "db", cfg.DB)
	st, err := store.Open(cfg.DB)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := seedTargets(ctx, st, cfg, logger); err != nil {
		return err
	}
	notifiers, err := loadNotifiers(ctx, st, cfg, logger)
	if err != nil {
		return err
	}

	mon := monitor.New(monitor.Options{Store: st, Notifiers: notifiers, Logger: logger, FailureThreshold: cfg.FailureThreshold})
	if err := mon.Start(ctx); err != nil {
		return err
	}
	srv, err := web.New(cfg, st, mon, logger, nil)
	if err != nil {
		return err
	}
	switch {
	case cfg.AdminPassword == "":
		logger.Warn("admin_password is empty: the dashboard is open to anyone who can reach it")
	case len(cfg.AdminPassword) < 12:
		logger.Warn("admin_password is short; use at least 12 characters")
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
func seedTargets(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger) error {
	for _, t := range cfg.Targets {
		if _, err := st.GetTargetByName(ctx, t.Name); err == nil {
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := st.CreateTarget(ctx, store.Target{Name: t.Name, URL: t.URL, Interval: t.Interval.Duration,
			Timeout: t.Timeout.Duration, ExpectStatus: t.ExpectStatus, Keyword: t.Keyword, Group: t.Group}, time.Now()); err != nil {
			return fmt.Errorf("seed target %q: %w", t.Name, err)
		}
		logger.Info("added target from config", "name", t.Name)
	}
	return nil
}

// loadNotifiers prefers settings saved from the UI; otherwise it uses the
// config file and stores that so the alerts page shows it.
func loadNotifiers(ctx context.Context, st *store.Store, cfg *config.Config, logger *slog.Logger) (notify.Multi, error) {
	raw, err := st.GetSetting(ctx, "notify")
	if err == nil {
		var n config.Notify
		if jerr := json.Unmarshal([]byte(raw), &n); jerr == nil {
			return notify.FromConfig(n, logger), nil
		}
		logger.Warn("stored notify settings are invalid; using config file")
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if b, err := json.Marshal(cfg.Notify); err == nil {
		if err := st.SetSetting(ctx, "notify", string(b)); err != nil {
			return nil, err
		}
	}
	return notify.FromConfig(cfg.Notify, logger), nil
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
