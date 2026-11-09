package store

import (
	"context"
	"errors"
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
	tg, err := s.CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: 30 * time.Second, Timeout: 5 * time.Second}, now)
	if err != nil || tg.ID == 0 {
		t.Fatalf("create: %v %+v", err, tg)
	}
	if _, err := s.CreateTarget(ctx, Target{Name: "A", URL: "https://b", Interval: time.Second, Timeout: time.Second}, now); err == nil {
		t.Fatal("duplicate name should fail")
	}
	got, err := s.GetTarget(ctx, tg.ID)
	if err != nil || got.Interval != 30*time.Second || got.Name != "A" {
		t.Fatalf("get: %v %+v", err, got)
	}
	got.Name, got.Keyword = "B", "ok"
	if err := s.UpdateTarget(ctx, got); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetTargetByName(ctx, "B"); g.Keyword != "ok" {
		t.Fatalf("update not persisted: %+v", g)
	}
	if err := s.SetPaused(ctx, tg.ID, true); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListTargets(ctx)
	if len(list) != 1 || !list[0].Paused {
		t.Fatalf("list: %+v", list)
	}
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now, OK: true, StatusCode: 200, LatencyMS: 10})
	id, _ := s.OpenIncident(ctx, tg.ID, now, "x")
	s.InsertAlert(ctx, Alert{IncidentID: id, TargetID: tg.ID, SentAt: now, Channel: "log", Event: "down", OK: true})
	if err := s.DeleteTarget(ctx, tg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTarget(ctx, tg.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if n, _ := s.CountAlerts(ctx, time.Time{}); n != 0 {
		t.Fatalf("alerts not cascaded: %d", n)
	}
	if err := s.UpdateTarget(ctx, got); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

func TestChecksStatsPrune(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, _ := s.CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
	for i := 0; i < 10; i++ {
		ok := i%5 != 0 // 2 failures
		s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(time.Duration(-i) * time.Minute), OK: ok, StatusCode: 200, LatencyMS: int64(100 + i)})
	}
	st, err := s.Stats(ctx, tg.ID, now.Add(-time.Hour))
	if err != nil || st.Total != 10 || st.OKCount != 8 {
		t.Fatalf("stats: %v %+v", err, st)
	}
	if st.UptimePercent() != 80 {
		t.Fatalf("uptime %v", st.UptimePercent())
	}
	all, _ := s.Stats(ctx, 0, now.Add(-time.Hour))
	if all.Total != 10 {
		t.Fatalf("global stats %+v", all)
	}
	latest, err := s.LatestCheck(ctx, tg.ID)
	if err != nil || !latest.CheckedAt.Equal(now) {
		t.Fatalf("latest: %v %+v", err, latest)
	}
	recent, _ := s.RecentChecks(ctx, tg.ID, 3)
	if len(recent) != 3 || !recent[0].CheckedAt.Equal(now) {
		t.Fatalf("recent: %+v", recent)
	}
	since, _ := s.ChecksSince(ctx, tg.ID, now.Add(-2*time.Minute))
	if len(since) != 3 || !since[0].CheckedAt.Before(since[2].CheckedAt) {
		t.Fatalf("since should be oldest first: %+v", since)
	}
	days, err := s.DailyUptime(ctx, tg.ID, now, 3)
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
	tg, _ := s.CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
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
	list, _ := s.ListIncidents(ctx, IncidentFilter{TargetID: tg.ID})
	if len(list) != 1 || list[0].Ongoing() || list[0].Duration(now) != 5*time.Minute {
		t.Fatalf("list: %+v", list)
	}
	if l, _ := s.ListIncidents(ctx, IncidentFilter{OngoingOnly: true}); len(l) != 0 {
		t.Fatalf("ongoing filter: %+v", l)
	}
	if n, _ := s.CountIncidents(ctx, now.Add(-time.Hour)); n != 1 {
		t.Fatalf("count %d", n)
	}
	if n, _ := s.CountAlerts(ctx, now.Add(-time.Hour)); n != 1 {
		t.Fatalf("alerts %d", n)
	}
}

func TestSettings(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if _, err := s.GetSetting(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.SetSetting(ctx, "k", "1")
	s.SetSetting(ctx, "k", "2")
	if v, _ := s.GetSetting(ctx, "k"); v != "2" {
		t.Fatalf("got %q", v)
	}
}

func TestMaintenanceWindows(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	if _, err := s.CreateMaintenance(ctx, Maintenance{Title: "bad", StartsAt: now, EndsAt: now}, now); err == nil {
		t.Fatal("zero-length window should be rejected")
	}
	all, err := s.CreateMaintenance(ctx, Maintenance{Title: "DB upgrade", StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}, now)
	if err != nil || all.ID == 0 {
		t.Fatalf("create: %v", err)
	}
	api, _ := s.CreateMaintenance(ctx, Maintenance{Title: "API deploy", StartsAt: now.Add(2 * time.Hour), EndsAt: now.Add(3 * time.Hour), Group: "api"}, now)
	old, _ := s.CreateMaintenance(ctx, Maintenance{Title: "Old", StartsAt: now.Add(-48 * time.Hour), EndsAt: now.Add(-47 * time.Hour)}, now)

	if !all.Active(now) || all.Upcoming(now) || !api.Upcoming(now) || !all.Covers("x") || api.Covers("web") || !api.Covers("api") {
		t.Fatal("window predicates wrong")
	}
	active, err := s.ActiveMaintenance(ctx, now)
	if err != nil || len(active) != 1 || active[0].ID != all.ID {
		t.Fatalf("active: %v %+v", err, active)
	}
	if m, ok, err := s.InMaintenance(ctx, "web", now); err != nil || !ok || m.ID != all.ID {
		t.Fatalf("in maintenance (all): %v %v", ok, err)
	}
	if _, ok, _ := s.InMaintenance(ctx, "web", now.Add(2*time.Hour+time.Minute)); ok {
		t.Fatal("web should not be covered by the api window")
	}
	if m, ok, _ := s.InMaintenance(ctx, "api", now.Add(2*time.Hour+time.Minute)); !ok || m.ID != api.ID {
		t.Fatal("api should be covered by the api window")
	}
	list, _ := s.ListMaintenance(ctx, now, 0)
	if len(list) != 2 || list[0].ID != all.ID || list[1].ID != api.ID {
		t.Fatalf("list since now: %+v", list)
	}
	if list, _ = s.ListMaintenance(ctx, time.Time{}, 0); len(list) != 3 || list[0].ID != old.ID {
		t.Fatalf("list all: %+v", list)
	}
	// End the active one now; a window that has not started cannot be ended.
	if err := s.EndMaintenance(ctx, all.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.EndMaintenance(ctx, api.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ending an upcoming window: %v", err)
	}
	if active, _ = s.ActiveMaintenance(ctx, now); len(active) != 0 {
		t.Fatalf("still active after end: %+v", active)
	}
	if err := s.DeleteMaintenance(ctx, api.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMaintenance(ctx, api.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted window still found: %v", err)
	}
}

func TestMaintenanceChecksExcludedFromUptime(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tg, _ := s.CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Second, Timeout: time.Second}, now)
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-3 * time.Minute), OK: true, LatencyMS: 100})
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-2 * time.Minute), OK: false, Maintenance: true})
	s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-1 * time.Minute), OK: true, LatencyMS: 900, Maintenance: true})
	st, err := s.Stats(ctx, tg.ID, now.Add(-time.Hour))
	if err != nil || st.Total != 1 || st.OKCount != 1 || st.AvgLatencyMS != 100 {
		t.Fatalf("stats should skip maintenance checks: %v %+v", err, st)
	}
	if days, _ := s.DailyUptime(ctx, tg.ID, now, 1); days[0].Total != 1 {
		t.Fatalf("daily: %+v", days)
	}
	if hours, _ := s.HourlyUptime(ctx, tg.ID, now, 2); hours[0].Total != 1 {
		t.Fatalf("hourly: %+v", hours)
	}
	if lat, _ := s.Latencies(ctx, tg.ID, now.Add(-time.Hour)); len(lat) != 1 || lat[0] != 100 {
		t.Fatalf("latencies: %v", lat)
	}
	// The raw history still has all three, flagged.
	recent, _ := s.RecentChecks(ctx, tg.ID, 10)
	if len(recent) != 3 || !recent[0].Maintenance || recent[2].Maintenance {
		t.Fatalf("recent: %+v", recent)
	}
}
