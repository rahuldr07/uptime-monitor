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
