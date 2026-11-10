package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTargetsCRUD(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, err := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: 30 * time.Second, Timeout: 5 * time.Second}, now)
	if err != nil || tg.ID == 0 {
		t.Fatalf("create: %v %+v", err, tg)
	}
	if _, err := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://b", Interval: time.Second, Timeout: time.Second}, now); err == nil {
		t.Fatal("duplicate name should fail")
	}
	got, err := s.Workspace(1).GetTarget(ctx, tg.ID)
	if err != nil || got.Interval != 30*time.Second || got.Name != "A" {
		t.Fatalf("get: %v %+v", err, got)
	}
	got.Name, got.Keyword = "B", "ok"
	if _, err := s.Workspace(1).UpdateTarget(ctx, got); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.Workspace(1).GetTargetByName(ctx, "B"); g.Keyword != "ok" {
		t.Fatalf("update not persisted: %+v", g)
	}
	if err := s.Workspace(1).SetPaused(ctx, tg.ID, true); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Workspace(1).ListTargets(ctx)
	if len(list) != 1 || !list[0].Paused {
		t.Fatalf("list: %+v", list)
	}
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now, OK: true, StatusCode: 200, LatencyMS: 10})
	id, _ := s.OpenIncident(ctx, tg.ID, now, "x")
	s.InsertAlert(ctx, Alert{IncidentID: id, TargetID: tg.ID, SentAt: now, Channel: "log", Event: "down", OK: true})
	if err := s.Workspace(1).DeleteTarget(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Workspace(1).GetTarget(ctx, tg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if n, _ := s.Workspace(1).CountAlerts(ctx, time.Time{}); n != 0 {
		t.Fatalf("alerts not cascaded: %d", n)
	}
	if _, err := s.Workspace(1).UpdateTarget(ctx, got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestChecksStatsPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, _ := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
	for i := 0; i < 10; i++ {
		ok := i%5 != 0 // 2 failures
		s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(time.Duration(-i) * time.Minute), OK: ok, StatusCode: 200, LatencyMS: int64(100 + i)})
	}
	st, err := s.Workspace(1).Stats(ctx, tg.ID, now.Add(-time.Hour))
	if err != nil || st.Total != 10 || st.OKCount != 8 {
		t.Fatalf("stats: %v %+v", err, st)
	}
	if st.UptimePercent() != 80 {
		t.Fatalf("uptime %v", st.UptimePercent())
	}
	all, _ := s.Workspace(1).Stats(ctx, 0, now.Add(-time.Hour))
	if all.Total != 10 {
		t.Fatalf("global stats %+v", all)
	}
	latest, err := s.LatestCheck(ctx, tg.ID)
	if err != nil || !latest.CheckedAt.Equal(now) {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	recent, _ := s.Workspace(1).RecentChecks(ctx, tg.ID, 3)
	if len(recent) != 3 || !recent[0].CheckedAt.Equal(now) {
		t.Fatalf("recent: %+v", recent)
	}
	since, _ := s.Workspace(1).ChecksSince(ctx, tg.ID, now.Add(-2*time.Minute))
	if len(since) != 3 || !since[0].CheckedAt.Before(since[2].CheckedAt) {
		t.Fatalf("since should be oldest first: %+v", since)
	}
	days, err := s.Workspace(1).DailyUptime(ctx, tg.ID, now, 3)
	if err != nil || len(days) != 3 || days[2].Total != 10 || days[0].Total != 0 {
		t.Fatalf("daily: %v %+v", err, days)
	}
	n, err := s.Prune(ctx, now.Add(-5*time.Minute))
	if err != nil || n != 4 {
		t.Fatalf("prune: %v %d", err, n)
	}
}

func TestIncidents(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, _ := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
	if _, err := s.OngoingIncident(ctx, tg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	id, err := s.OpenIncident(ctx, tg.ID, now, "HTTP 503")
	if err != nil {
		t.Fatal(err)
	}
	s.InsertAlert(ctx, Alert{IncidentID: id, TargetID: tg.ID, SentAt: now, Channel: "slack", Event: "down", OK: true})
	s.InsertAlert(ctx, Alert{IncidentID: id, TargetID: tg.ID, SentAt: now, Channel: "email", Event: "down", OK: false, Error: "x"})
	in, err := s.OngoingIncident(ctx, tg.ID)
	if err != nil || in.ID != id || in.Channels != "slack" || in.TargetName != "A" {
		t.Fatalf("ongoing: %v %+v", err, in)
	}
	if err := s.CloseIncident(ctx, id, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIncident(ctx, id, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double close should be not found: %v", err)
	}
	list, _ := s.Workspace(1).ListIncidents(ctx, IncidentFilter{TargetID: tg.ID})
	if len(list) != 1 || list[0].Ongoing() || list[0].Duration(now) != 5*time.Minute {
		t.Fatalf("list: %+v", list)
	}
	if l, _ := s.Workspace(1).ListIncidents(ctx, IncidentFilter{OngoingOnly: true}); len(l) != 0 {
		t.Fatalf("ongoing filter: %+v", l)
	}
	if n, _ := s.Workspace(1).CountIncidents(ctx, now.Add(-time.Hour)); n != 1 {
		t.Fatalf("count %d", n)
	}
	if n, _ := s.Workspace(1).CountAlerts(ctx, now.Add(-time.Hour)); n != 1 {
		t.Fatalf("alerts %d", n)
	}
}

func TestSettings(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.Workspace(1).GetSetting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.Workspace(1).SetSetting(ctx, "k", "1")
	s.Workspace(1).SetSetting(ctx, "k", "2")
	if v, _ := s.Workspace(1).GetSetting(ctx, "k"); v != "2" {
		t.Fatalf("got %q", v)
	}
}

func TestMaintenanceWindows(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if _, err := s.Workspace(1).CreateMaintenance(ctx, Maintenance{Title: "bad", StartsAt: now, EndsAt: now}, now); err == nil {
		t.Fatal("zero-length window should be rejected")
	}
	all, err := s.Workspace(1).CreateMaintenance(ctx, Maintenance{Title: "DB upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}, now)
	if err != nil || all.ID == 0 {
		t.Fatalf("create: %v", err)
	}
	api, _ := s.Workspace(1).CreateMaintenance(ctx, Maintenance{Title: "API deploy", StartsAt: now.Add(2 * time.Hour), EndsAt: now.Add(3 * time.Hour), Group: "api"}, now)
	old, _ := s.Workspace(1).CreateMaintenance(ctx, Maintenance{Title: "Old", StartsAt: now.Add(-48 * time.Hour), EndsAt: now.Add(-47 * time.Hour)}, now)

	if !all.Active(now) || all.Upcoming(now) || !api.Upcoming(now) || !all.Covers("x") || api.Covers("web") || !api.Covers("api") {
		t.Fatal("window predicates wrong")
	}
	active, err := s.Workspace(1).ActiveMaintenance(ctx, now)
	if err != nil || len(active) != 1 || active[0].ID != all.ID {
		t.Fatalf("active: %v %+v", err, active)
	}
	if m, ok, err := s.Workspace(1).InMaintenance(ctx, "web", now); err != nil || !ok || m.ID != all.ID {
		t.Fatalf("in maintenance (all): %v %v", ok, err)
	}
	if _, ok, _ := s.Workspace(1).InMaintenance(ctx, "web", now.Add(2*time.Hour+time.Minute)); ok {
		t.Fatal("web should not be covered by the api window")
	}
	if m, ok, _ := s.Workspace(1).InMaintenance(ctx, "api", now.Add(2*time.Hour+time.Minute)); !ok || m.ID != api.ID {
		t.Fatal("api should be covered by the api window")
	}
	list, _ := s.Workspace(1).ListMaintenance(ctx, now, 0)
	if len(list) != 2 || list[0].ID != all.ID || list[1].ID != api.ID {
		t.Fatalf("list since now: %+v", list)
	}
	if list, _ = s.Workspace(1).ListMaintenance(ctx, time.Time{}, 0); len(list) != 3 || list[0].ID != old.ID {
		t.Fatalf("list all: %+v", list)
	}
	// End the active one now; a window that has not started cannot be ended.
	if err := s.Workspace(1).EndMaintenance(ctx, all.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Workspace(1).EndMaintenance(ctx, api.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending an upcoming window: %v", err)
	}
	if active, _ = s.Workspace(1).ActiveMaintenance(ctx, now); len(active) != 0 {
		t.Fatalf("still active after end: %+v", active)
	}
	if err := s.Workspace(1).DeleteMaintenance(ctx, api.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Workspace(1).GetMaintenance(ctx, api.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted window still found: %v", err)
	}
}

func TestMaintenanceChecksExcludedFromUptime(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, _ := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-3 * time.Minute), OK: true, LatencyMS: 100})
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-2 * time.Minute), OK: false, Maintenance: true})
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-1 * time.Minute), OK: true, LatencyMS: 900, Maintenance: true})
	st, err := s.Workspace(1).Stats(ctx, tg.ID, now.Add(-time.Hour))
	if err != nil || st.Total != 1 || st.OKCount != 1 || st.AvgLatencyMS != 100 {
		t.Fatalf("stats should skip maintenance checks: %v %+v", err, st)
	}
	if days, _ := s.Workspace(1).DailyUptime(ctx, tg.ID, now, 1); days[0].Total != 1 {
		t.Fatalf("daily: %+v", days)
	}
	if hours, _ := s.Workspace(1).HourlyUptime(ctx, tg.ID, now, 2); hours[0].Total != 1 {
		t.Fatalf("hourly: %+v", hours)
	}
	if lat, _ := s.Workspace(1).Latencies(ctx, tg.ID, now.Add(-time.Hour)); len(lat) != 1 || lat[0] != 100 {
		t.Fatalf("latencies: %v", lat)
	}
	// The raw history still has all three, flagged.
	recent, _ := s.Workspace(1).RecentChecks(ctx, tg.ID, 10)
	if len(recent) != 3 || !recent[0].Maintenance || recent[2].Maintenance {
		t.Fatalf("recent: %+v", recent)
	}
}

func TestRollupsOutliveRawChecks(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, _ := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
	// 20 days of history: one ok and one failed check per day, plus one
	// maintenance failure that must not count.
	for d := 0; d < 20; d++ {
		at := now.AddDate(0, 0, -d)
		s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: at, OK: true, LatencyMS: 100})
		s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: at.Add(time.Minute), OK: false})
		s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: at.Add(2 * time.Minute), OK: false, Maintenance: true})
	}
	// Raw checks are kept for 7 days only.
	if _, err := s.Prune(ctx, now.AddDate(0, 0, -7)); err != nil {
		t.Fatal(err)
	}
	st, err := s.Workspace(1).RollupStats(ctx, tg.ID, now.AddDate(0, 0, -29))
	if err != nil || st.Total != 40 || st.OKCount != 20 || st.AvgLatencyMS != 100 {
		t.Fatalf("30d rollup after prune: %v %+v", err, st)
	}
	days, _ := s.Workspace(1).DailyUptime(ctx, tg.ID, now, 90)
	filled := 0
	for _, d := range days {
		if d.Total > 0 {
			filled++
		}
	}
	if filled != 20 || days[89].Total != 2 {
		t.Fatalf("90-day history should keep all 20 days after prune, got %d", filled)
	}
	if all, _ := s.Workspace(1).RollupStats(ctx, 0, now.AddDate(0, 0, -29)); all.Total != 40 {
		t.Fatalf("all-target rollup: %+v", all)
	}
	// Days 11..19 ago end before the cutoff day; day 10 ago is kept.
	if n, err := s.PruneRollups(ctx, now.AddDate(0, 0, -10)); err != nil || n != 9 {
		t.Fatalf("prune rollups: %v %d", err, n)
	}
	if err := s.Workspace(1).DeleteTarget(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Workspace(1).RollupStats(ctx, 0, time.Time{}); st.Total != 0 {
		t.Fatalf("rollups should be deleted with the target: %+v", st)
	}
}

func TestRollupBackfillAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// Build the database as a v1 one: the workspace-aware step 2 is not
	// idempotent, so it must not have run before step 1 is replayed.
	full := steps
	steps = full[:1]
	s, err := Open(path)
	steps = full
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if _, err := s.db.Exec(`INSERT INTO targets (name, url, interval_ms, timeout_ms, created_at) VALUES ('A', 'https://a', 1000, 1000, ?)`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	tg := Target{ID: 1}
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now, OK: true, LatencyMS: 50})
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-24 * time.Hour), OK: false})
	// Simulate a database from before rollups existed.
	if _, err := s.db.Exec(`DROP TABLE daily`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if st, _ := s.Workspace(1).RollupStats(ctx, tg.ID, now.AddDate(0, 0, -7)); st.Total != 2 || st.OKCount != 1 {
		t.Fatalf("rollups should be backfilled from existing checks: %+v", st)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("database file should be 0600, got %o", fi.Mode().Perm())
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "backup.db")
	if err := Backup(ctx, path, dest); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, path, dest); err == nil {
		t.Fatal("backup must not overwrite an existing file")
	}
	// A wrong source path must fail and must not create anything.
	missing := filepath.Join(dir, "nope.db")
	if err := Backup(ctx, missing, filepath.Join(dir, "b2.db")); err == nil {
		t.Fatal("backing up a missing database must fail")
	}
	for _, p := range []string{missing, filepath.Join(dir, "b2.db")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s should not have been created", p)
		}
	}
	notDB := filepath.Join(dir, "empty.db")
	os.WriteFile(notDB, nil, 0o600)
	if err := Backup(ctx, notDB, filepath.Join(dir, "b3.db")); err == nil {
		t.Fatal("an empty file is not a database worth backing up")
	}
	b, err := Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if got, _ := b.Workspace(1).GetTarget(ctx, tg.ID); got.Name != "A" {
		t.Fatalf("backup should contain the data: %+v", got)
	}
	if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup should be 0600, got %o", fi.Mode().Perm())
	}
}
