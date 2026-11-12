package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

var adminNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func adminAccount(t *testing.T, s *Store, email, slug string, now time.Time) (User, *WS) {
	t.Helper()
	u, ws, err := s.CreateAccount(context.Background(), AccountInput{Email: email, PasswordHash: "x", WorkspaceName: slug, Slug: slug, Verified: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return u, ws
}

func adminTarget(t *testing.T, ws *WS, name, url string) Target {
	t.Helper()
	tg, err := ws.CreateTarget(context.Background(), Target{Name: name, URL: url, Interval: time.Minute, Timeout: time.Second}, adminNow)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func count(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSetUserDisabled(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, ws := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	adminTarget(t, ws, "A", "https://a.example")
	if _, err := s.CreateSession(ctx, u.ID, adminNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, TokenVerify, u.ID, time.Hour, adminNow); err != nil {
		t.Fatal(err)
	}

	ids, err := s.SetUserDisabled(ctx, u.ID, true, adminNow)
	if err != nil || !slices.Equal(ids, []int64{ws.ID()}) {
		t.Fatalf("disable = %v, %v", ids, err)
	}
	got, err := s.UserByID(ctx, u.ID)
	if err != nil || got.DisabledAt == nil || !got.DisabledAt.Equal(adminNow) {
		t.Fatalf("disabled_at: %+v %v", got, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID); n != 0 {
		t.Errorf("sessions left: %d", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM tokens WHERE user_id = ?`, u.ID); n != 0 {
		t.Errorf("tokens left: %d", n)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM workspaces WHERE id = ? AND suspended_at IS NOT NULL`, ws.ID()); n != 1 {
		t.Errorf("workspace not suspended")
	}

	ids, err = s.SetUserDisabled(ctx, u.ID, false, adminNow.Add(time.Hour))
	if err != nil || !slices.Equal(ids, []int64{ws.ID()}) {
		t.Fatalf("enable = %v, %v", ids, err)
	}
	if got, _ := s.UserByID(ctx, u.ID); got.DisabledAt != nil {
		t.Errorf("disabled_at not cleared")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM workspaces WHERE id = ? AND suspended_at IS NOT NULL`, ws.ID()); n != 0 {
		t.Errorf("workspace still suspended")
	}
	if _, err := s.SetUserDisabled(ctx, 9999, true, adminNow); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestSetUserDisabledSharedWorkspaceUntouched(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, ws := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	other, _ := adminAccount(t, s, "b@x.example", "bbb", adminNow)
	if _, err := s.db.Exec(`INSERT INTO memberships (user_id, workspace_id, role, created_at) VALUES (?, ?, 'viewer', ?)`, other.ID, ws.ID(), adminNow.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	ids, err := s.SetUserDisabled(ctx, u.ID, true, adminNow)
	if err != nil || len(ids) != 0 {
		t.Fatalf("ids = %v, %v", ids, err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM workspaces WHERE suspended_at IS NOT NULL`); n != 0 {
		t.Errorf("shared workspace suspended")
	}
}

func TestSetUserDisabledOperator(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	tok, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, adminNow)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.CompleteSetup(ctx, SetupInput{Email: "op@x.example", PasswordHash: "x", WorkspaceName: "Op", SetupToken: tok, Now: adminNow})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserDisabled(ctx, op.ID, true, adminNow); !errors.Is(err, ErrOperator) {
		t.Fatalf("disable operator: %v", err)
	}
	if got, _ := s.UserByID(ctx, op.ID); got.DisabledAt != nil {
		t.Errorf("operator was disabled")
	}
	if _, err := s.db.Exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, adminNow.UnixMilli(), op.ID); err != nil {
		t.Fatal(err)
	}
	ids, err := s.SetUserDisabled(ctx, op.ID, false, adminNow)
	if err != nil {
		t.Fatalf("enable operator: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("enable operator returned workspaces: %v", ids)
	}
	if got, _ := s.UserByID(ctx, op.ID); got.DisabledAt != nil {
		t.Errorf("operator still disabled")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM workspaces WHERE id = 1 AND suspended_at IS NOT NULL`); n != 0 {
		t.Errorf("workspace 1 suspended")
	}
}

func TestSuspendedHiddenFromEngine(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, ws := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	tg := adminTarget(t, ws, "A", "https://a.example")
	other, ows := adminAccount(t, s, "b@x.example", "bbb", adminNow)
	_ = other
	adminTarget(t, ows, "B", "https://b.example")

	visible := func(want bool) {
		t.Helper()
		all, err := s.AllTargets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		found := slices.ContainsFunc(all, func(x Target) bool { return x.ID == tg.ID })
		if found != want {
			t.Errorf("AllTargets has target = %v, want %v", found, want)
		}
		got, err := s.TargetByID(ctx, tg.ID)
		if want && (err != nil || got.ID != tg.ID || got.Name != "A" || got.WorkspaceID != ws.ID()) {
			t.Errorf("TargetByID = %+v, %v", got, err)
		}
		if !want && !errors.Is(err, ErrNotFound) {
			t.Errorf("TargetByID err = %v", err)
		}
		_, err = s.WorkspaceBySlug(ctx, "aaa")
		if want && err != nil || !want && !errors.Is(err, ErrNotFound) {
			t.Errorf("WorkspaceBySlug err = %v, want visible=%v", err, want)
		}
		others, err := s.TargetsInOtherWorkspaces(ctx, ows.ID())
		if err != nil {
			t.Fatal(err)
		}
		found = slices.ContainsFunc(others, func(x Target) bool { return x.ID == tg.ID })
		if found != want {
			t.Errorf("TargetsInOtherWorkspaces has target = %v, want %v", found, want)
		}
	}
	visible(true)
	if _, err := s.SetUserDisabled(ctx, u.ID, true, adminNow); err != nil {
		t.Fatal(err)
	}
	visible(false)
	if _, err := s.SetUserDisabled(ctx, u.ID, false, adminNow); err != nil {
		t.Fatal(err)
	}
	visible(true)
}

func TestListUsers(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	var first User
	var firstWS *WS
	for i := range 55 {
		u, ws := adminAccount(t, s, fmt.Sprintf("u%02d@x.example", i), fmt.Sprintf("ws-%02d", i), adminNow.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			first, firstWS = u, ws
		}
	}
	adminTarget(t, firstWS, "one", "https://one.example")
	adminTarget(t, firstWS, "two", "https://two.example")

	rows, total, err := s.ListUsers(ctx, UserQuery{})
	if err != nil || total != 55 || len(rows) != 50 {
		t.Fatalf("page 0: %d rows, total %d, %v", len(rows), total, err)
	}
	if rows[0].Email != "u54@x.example" || rows[49].Email != "u05@x.example" {
		t.Errorf("order: %s ... %s", rows[0].Email, rows[49].Email)
	}
	if rows[0].WorkspaceSlug != "ws-54" || rows[0].WorkspaceID == 0 || rows[0].WorkspaceName != "ws-54" || rows[0].LastSeenAt != nil {
		t.Errorf("row 0: %+v", rows[0])
	}
	rows, total, err = s.ListUsers(ctx, UserQuery{Page: 1})
	if err != nil || total != 55 || len(rows) != 5 {
		t.Fatalf("page 1: %d rows, total %d, %v", len(rows), total, err)
	}
	last := rows[4]
	if last.ID != first.ID || last.Monitors != 2 || last.WorkspaceID != firstWS.ID() {
		t.Errorf("last row: %+v", last)
	}
	if rows[0].Monitors != 0 {
		t.Errorf("monitors = %d", rows[0].Monitors)
	}
	if rows, _, _ := s.ListUsers(ctx, UserQuery{Page: 9}); len(rows) != 0 {
		t.Errorf("page 9: %d rows", len(rows))
	}
}

func TestListUsersLastSeen(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _ := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	raw, err := s.CreateSession(ctx, u.ID, adminNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, raw, adminNow.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.ListUsers(ctx, UserQuery{})
	if err != nil || len(rows) != 1 || rows[0].LastSeenAt == nil || !rows[0].LastSeenAt.Equal(adminNow.Add(10*time.Minute)) {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
}

func TestListUsersSearchEscapes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	pct, pws := adminAccount(t, s, "50%_off@x.example", "pct", adminNow)
	adminAccount(t, s, "500off@x.example", "num", adminNow)
	adminTarget(t, pws, "api", "https://api.example.com/x")

	rows, total, err := s.ListUsers(ctx, UserQuery{Email: "50%_"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != pct.ID {
		t.Fatalf("email search: %+v total %d %v", rows, total, err)
	}
	rows, total, err = s.ListUsers(ctx, UserQuery{Host: "api.example"})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != pct.ID || rows[0].Monitors != 1 {
		t.Fatalf("host search: %+v total %d %v", rows, total, err)
	}
	if rows, _, _ := s.ListUsers(ctx, UserQuery{Host: "ap_.example"}); len(rows) != 0 {
		t.Errorf("underscore matched as wildcard: %+v", rows)
	}
	if rows, _, _ := s.ListUsers(ctx, UserQuery{Email: `\`}); len(rows) != 0 {
		t.Errorf("backslash matched: %+v", rows)
	}
}

func TestLikeContainsEscapes(t *testing.T) {
	if got, want := likeContains(`a\b%_`), `%a\\b\%\_%`; got != want {
		t.Errorf("likeContains = %q, want %q", got, want)
	}
}

func TestSetUserDisabledKeepsFirstTime(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, ws := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	if _, err := s.SetUserDisabled(ctx, u.ID, true, adminNow); err != nil {
		t.Fatal(err)
	}
	later := adminNow.Add(time.Hour)
	if _, err := s.SetUserDisabled(ctx, u.ID, true, later); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT COUNT(*) FROM users WHERE id = ? AND disabled_at = ?`, u.ID, adminNow.UnixMilli()); n != 1 {
		t.Errorf("disabled_at changed on second disable")
	}
	if n := count(t, s, `SELECT COUNT(*) FROM workspaces WHERE id = ? AND suspended_at = ?`, ws.ID(), adminNow.UnixMilli()); n != 1 {
		t.Errorf("suspended_at changed on second disable")
	}
}

func TestUserByID(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _ := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	got, err := s.UserByID(ctx, u.ID)
	if err != nil || got.Email != "a@x.example" || got.ID != u.ID {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := s.UserByID(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestListInvites(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	ttl := 7 * 24 * time.Hour
	if _, err := s.CreateInvite(ctx, "", ttl, adminNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "Bound@X.example", ttl, adminNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInvite(ctx, "", time.Minute, adminNow.Add(-time.Hour)); err != nil { // expired
		t.Fatal(err)
	}
	used, err := s.CreateInvite(ctx, "", ttl, adminNow.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumeToken(ctx, s.db, TokenInvite, used, adminNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, adminNow); err != nil {
		t.Fatal(err)
	}

	inv, err := s.ListInvites(ctx, adminNow)
	if err != nil || len(inv) != 2 {
		t.Fatalf("invites = %+v, %v", inv, err)
	}
	if inv[0].Email != "bound@x.example" || inv[1].Email != "" {
		t.Errorf("order/email: %+v", inv)
	}
	if !inv[0].CreatedAt.Equal(adminNow) || !inv[0].ExpiresAt.Equal(adminNow.Add(ttl)) || len(inv[0].ID) != 64 {
		t.Errorf("fields: %+v", inv[0])
	}
}

func TestRevokeInvite(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	raw, err := s.CreateInvite(ctx, "", time.Hour, adminNow)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := adminAccount(t, s, "a@x.example", "aaa", adminNow)
	reset, err := s.CreateToken(ctx, TokenReset, u.ID, time.Hour, adminNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, hashSecret(reset)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset hash: %v", err)
	}
	if _, err := s.PeekToken(ctx, TokenReset, reset, adminNow); err != nil {
		t.Fatalf("reset token damaged: %v", err)
	}
	if err := s.RevokeInvite(ctx, hashSecret(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekToken(ctx, TokenInvite, raw, adminNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("peek after revoke: %v", err)
	}
	if err := s.RevokeInvite(ctx, hashSecret(raw)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestPruneSkipsDisabled(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	old := adminNow.Add(-49 * time.Hour)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.example", PasswordHash: "x", WorkspaceName: "A", Slug: "aaa", Now: old})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserDisabled(ctx, u.ID, true, adminNow); err != nil {
		t.Fatal(err)
	}
	ids, err := s.PruneUnverified(ctx, adminNow.Add(-48*time.Hour))
	if err != nil || len(ids) != 0 {
		t.Fatalf("prune = %v, %v", ids, err)
	}
	if _, err := s.UserByID(ctx, u.ID); err != nil {
		t.Fatalf("disabled user pruned: %v", err)
	}
}
