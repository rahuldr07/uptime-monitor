package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// wsFixture holds the ids of the rows seeded into one workspace.
type wsFixture struct {
	target   Target
	incident int64
	maint    Maintenance
}

func seedWS(t *testing.T, s *Store, w *WS, name string, now time.Time) wsFixture {
	t.Helper()
	ctx := context.Background()
	tg, err := w.CreateTarget(ctx, Target{Name: name, URL: "https://" + name, Interval: time.Minute, Timeout: time.Second}, now)
	if err != nil {
		t.Fatal(err)
	}
	for i, ok := range []bool{true, false} {
		err := s.InsertCheck(ctx, Check{TargetID: tg.ID, CheckedAt: now.Add(-time.Duration(i) * time.Minute), OK: ok, StatusCode: 200, LatencyMS: 50})
		if err != nil {
			t.Fatal(err)
		}
	}
	inc, err := s.OpenIncident(ctx, tg.ID, now, "boom")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertAlert(ctx, Alert{IncidentID: inc, TargetID: tg.ID, SentAt: now, Channel: "slack", Event: "down", OK: true}); err != nil {
		t.Fatal(err)
	}
	m, err := w.CreateMaintenance(ctx, Maintenance{Title: name, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetSetting(ctx, "notify", `{"slack":{"webhook_url":"https://hooks/`+name+`"}}`); err != nil {
		t.Fatal(err)
	}
	return wsFixture{tg, inc, m}
}

type wsCounts struct {
	stats, rollup           Stats
	incidents, alerts, unck int
	last                    int
	maint                   int
}

func countsOf(t *testing.T, w *WS, now time.Time) wsCounts {
	t.Helper()
	ctx := context.Background()
	since := now.Add(-24 * time.Hour)
	var c wsCounts
	var err error
	if c.stats, err = w.Stats(ctx, 0, since); err != nil {
		t.Fatal(err)
	}
	if c.rollup, err = w.RollupStats(ctx, 0, since); err != nil {
		t.Fatal(err)
	}
	if c.incidents, err = w.CountIncidents(ctx, since); err != nil {
		t.Fatal(err)
	}
	if c.alerts, err = w.CountAlerts(ctx, since); err != nil {
		t.Fatal(err)
	}
	if c.unck, err = w.CountUnacknowledged(ctx); err != nil {
		t.Fatal(err)
	}
	last, err := w.LastAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.last = len(last)
	ms, err := w.ListMaintenance(ctx, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c.maint = len(ms)
	return c
}

func TestWSIsolation(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ws2, err := createWorkspace(ctx, s.db, "B", "b", now)
	if err != nil {
		t.Fatal(err)
	}
	a, b := s.Workspace(1), s.Workspace(ws2.ID)

	fb := seedWS(t, s, b, "b", now)
	bOnly := countsOf(t, b, now)
	if bOnly.stats.Total != 2 || bOnly.incidents != 1 || bOnly.alerts != 1 || bOnly.maint != 1 || bOnly.last != 1 {
		t.Fatalf("b baseline wrong: %+v", bOnly)
	}
	fa := seedWS(t, s, a, "a", now)
	if got := countsOf(t, b, now); got != bOnly {
		t.Fatalf("b aggregates changed after seeding a: before %+v after %+v", bOnly, got)
	}
	if aC := countsOf(t, a, now); aC != bOnly {
		t.Fatalf("a should see only its own identical-shaped rows: %+v", aC)
	}

	// Both workspaces alert on "slack"; a's alert is the newest row overall,
	// so an unscoped MAX(id) would hand it to b.
	if last, _ := b.LastAlerts(ctx); last["slack"].TargetID != fb.target.ID {
		t.Fatalf("b.LastAlerts returned %+v", last["slack"])
	}

	notFound := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want ErrNotFound, got %v", what, err)
		}
	}
	_, err = b.GetTarget(ctx, fa.target.ID)
	notFound("GetTarget", err)
	_, err = b.GetTargetByName(ctx, "a")
	notFound("GetTargetByName", err)
	_, err = b.GetIncident(ctx, fa.incident)
	notFound("GetIncident", err)
	_, err = b.GetMaintenance(ctx, fa.maint.ID)
	notFound("GetMaintenance", err)
	_, err = b.UpdateTarget(ctx, Target{ID: fa.target.ID, Name: "hijacked", URL: "https://x", Interval: time.Minute, Timeout: time.Second})
	notFound("UpdateTarget", err)
	notFound("SetPaused", b.SetPaused(ctx, fa.target.ID, true))
	notFound("SetMuted", b.SetMuted(ctx, fa.target.ID, true))
	notFound("AcknowledgeIncident", b.AcknowledgeIncident(ctx, fa.incident, now))
	notFound("SetIncidentNote", b.SetIncidentNote(ctx, fa.incident, "pwned"))
	notFound("EndMaintenance", b.EndMaintenance(ctx, fa.maint.ID, now))
	notFound("DeleteMaintenance", b.DeleteMaintenance(ctx, fa.maint.ID))
	notFound("DeleteTarget", b.DeleteTarget(ctx, fa.target.ID))

	// a's rows are untouched.
	tg, err := a.GetTarget(ctx, fa.target.ID)
	if err != nil || tg.Name != "a" || tg.Paused || tg.Muted {
		t.Fatalf("a's target changed: %v %+v", err, tg)
	}
	in, err := a.GetIncident(ctx, fa.incident)
	if err != nil || in.AckedAt != nil || in.Note != "" {
		t.Fatalf("a's incident changed: %v %+v", err, in)
	}
	m, err := a.GetMaintenance(ctx, fa.maint.ID)
	if err != nil || !m.EndsAt.Equal(fa.maint.EndsAt) {
		t.Fatalf("a's maintenance changed: %v %+v", err, m)
	}
	if cs, _ := a.RecentChecks(ctx, fa.target.ID, 10); len(cs) != 2 {
		t.Fatalf("a's checks lost: %d", len(cs))
	}

	// Reads of a's target through b are empty.
	since := now.Add(-24 * time.Hour)
	if cs, _ := b.RecentChecks(ctx, fa.target.ID, 10); len(cs) != 0 {
		t.Errorf("RecentChecks leaked %d", len(cs))
	}
	if cs, _ := b.ChecksSince(ctx, fa.target.ID, since); len(cs) != 0 {
		t.Errorf("ChecksSince leaked %d", len(cs))
	}
	if ls, _ := b.Latencies(ctx, fa.target.ID, since); len(ls) != 0 {
		t.Errorf("Latencies leaked %d", len(ls))
	}
	if st, _ := b.Stats(ctx, fa.target.ID, since); st.Total != 0 {
		t.Errorf("Stats(a's target) leaked %+v", st)
	}
	if st, _ := b.RollupStats(ctx, fa.target.ID, since); st.Total != 0 {
		t.Errorf("RollupStats(a's target) leaked %+v", st)
	}
	days, _ := b.DailyUptime(ctx, fa.target.ID, now, 3)
	hours, _ := b.HourlyUptime(ctx, fa.target.ID, now, 3)
	for _, st := range append(days, hours...) {
		if st.Total != 0 {
			t.Errorf("uptime series leaked %+v", st)
		}
	}
	if is, _ := b.ListIncidents(ctx, IncidentFilter{TargetID: fa.target.ID}); len(is) != 0 {
		t.Errorf("ListIncidents(a's target) leaked %d", len(is))
	}
	if is, _ := b.ListIncidents(ctx, IncidentFilter{}); len(is) != 1 || is[0].ID != fb.incident {
		t.Errorf("ListIncidents sees %+v", is)
	}
	if ts, _ := b.ListTargets(ctx); len(ts) != 1 || ts[0].ID != fb.target.ID {
		t.Errorf("ListTargets sees %+v", ts)
	}
	if ms, _ := b.ActiveMaintenance(ctx, now); len(ms) != 1 || ms[0].ID != fb.maint.ID {
		t.Errorf("ActiveMaintenance sees %+v", ms)
	}

	// InMaintenance only honours b's own windows.
	if _, ok, _ := b.InMaintenance(ctx, "", now); !ok {
		t.Error("b should be in maintenance via its own window")
	}
	if err := b.DeleteMaintenance(ctx, fb.maint.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.InMaintenance(ctx, "", now); ok {
		t.Error("b must not be in maintenance through a's window")
	}
	if _, ok, _ := a.InMaintenance(ctx, "", now); !ok {
		t.Error("a's window should still be active")
	}

	// Settings and Notify.
	n, err := b.Notify(ctx)
	if err != nil || n.Slack == nil || n.Slack.WebhookURL != "https://hooks/b" {
		t.Fatalf("b.Notify = %+v, %v", n, err)
	}
	if err := b.DeleteTarget(ctx, fb.target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetSetting(ctx, "notify"); err != nil {
		t.Fatalf("a's setting lost: %v", err)
	}
	if n, err := s.Workspace(ws2.ID).Notify(ctx); err != nil || n.Slack == nil {
		t.Fatalf("settings should outlive target deletion: %+v %v", n, err)
	}

	// CreateTarget ignores a caller-supplied workspace id.
	made, err := b.CreateTarget(ctx, Target{WorkspaceID: 1, Name: "x", URL: "https://x", Interval: time.Minute, Timeout: time.Second}, now)
	if err != nil || made.WorkspaceID != ws2.ID {
		t.Fatalf("CreateTarget: %v %+v", err, made)
	}
	if got, _ := b.GetTarget(ctx, made.ID); got.WorkspaceID != ws2.ID {
		t.Fatalf("stored WorkspaceID = %d", got.WorkspaceID)
	}

	// UpdateTarget returns the stored row; names are unique per workspace.
	upd, err := a.UpdateTarget(ctx, Target{ID: fa.target.ID, Name: "a2", URL: "https://a2", Interval: 2 * time.Minute, Timeout: time.Second})
	if err != nil || upd.Name != "a2" || upd.WorkspaceID != 1 || upd.Interval != 2*time.Minute {
		t.Fatalf("UpdateTarget: %v %+v", err, upd)
	}
	if _, err := b.CreateTarget(ctx, Target{Name: "a2", URL: "https://a2", Interval: time.Minute, Timeout: time.Second}, now); err != nil {
		t.Fatalf("same name in another workspace must be allowed: %v", err)
	}
}

func TestWSNotifyEmpty(t *testing.T) {
	s := open(t)
	n, err := s.Workspace(1).Notify(context.Background())
	if err != nil || n.Slack != nil || n.Telegram != nil || n.Email != nil {
		t.Fatalf("want zero Notify, got %+v %v", n, err)
	}
}

func TestWorkspaceLookups(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	ws, err := s.WorkspaceBySlug(ctx, "default")
	if err != nil || ws.ID != 1 || ws.Name != "Default" {
		t.Fatalf("default: %v %+v", err, ws)
	}
	if _, err := s.WorkspaceBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nope: %v", err)
	}
	info, err := s.Workspace(1).Info(ctx)
	if err != nil || info != ws {
		t.Fatalf("Info: %v %+v", err, info)
	}

	func() {
		defer func() {
			if r := recover(); r != "store: invalid workspace id" {
				t.Errorf("Workspace(0) panic = %v", r)
			}
		}()
		s.Workspace(0)
	}()

	ws2, err := createWorkspace(ctx, s.db, "B", "b", now)
	if err != nil {
		t.Fatal(err)
	}
	ta, _ := s.Workspace(1).CreateTarget(ctx, Target{Name: "a", URL: "https://a", Interval: time.Minute, Timeout: time.Second}, now)
	tb, _ := s.Workspace(ws2.ID).CreateTarget(ctx, Target{Name: "b", URL: "https://b", Interval: time.Minute, Timeout: time.Second}, now)
	all, err := s.AllTargets(ctx)
	if err != nil || len(all) != 2 || all[0].WorkspaceID != 1 || all[1].WorkspaceID != ws2.ID {
		t.Fatalf("AllTargets: %v %+v", err, all)
	}
	for _, want := range []Target{ta, tb} {
		got, err := s.TargetByID(ctx, want.ID)
		if err != nil || got.WorkspaceID != want.WorkspaceID || got.Name != want.Name {
			t.Fatalf("TargetByID(%d): %v %+v", want.ID, err, got)
		}
	}
	if _, err := s.TargetByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TargetByID missing: %v", err)
	}
}
