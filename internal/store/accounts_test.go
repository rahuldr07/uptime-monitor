package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTokensSingleUseKindAndExpiry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	raw, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, `SELECT token_hash FROM tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw || len(stored) != 64 || stored != hashSecret(raw) {
		t.Fatalf("stored value is not the hash: %q", stored)
	}
	tok, err := s.PeekToken(ctx, TokenSetup, raw, now.Add(time.Minute))
	if err != nil || tok.Kind != TokenSetup {
		t.Fatalf("peek: %v %+v", err, tok)
	}
	if _, err := s.PeekToken(ctx, TokenSetup, raw, now.Add(2*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired peek: %v", err)
	}
	if _, err := s.PeekToken(ctx, TokenReset, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong kind peek: %v", err)
	}

	// A new setup token deletes the previous unused one.
	raw2, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekToken(ctx, TokenSetup, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token still valid: %v", err)
	}

	// Exactly one of two concurrent consumers wins.
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = consumeToken(ctx, s.db, TokenSetup, raw2, now)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if ok != 1 {
		t.Fatalf("consumers succeeded = %d, want 1", ok)
	}
}

func TestResetTokenDisabledUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateToken(ctx, TokenReset, u.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, now.UnixMilli(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekToken(ctx, TokenReset, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("peek: %v", err)
	}
	if _, err := consumeToken(ctx, s.db, TokenReset, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("consume: %v", err)
	}
}

func TestCompleteSetup(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	raw, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	in := SetupInput{Email: "op@example.com", PasswordHash: "phc", WorkspaceName: "Acme", SetupToken: raw, Now: now}
	u, err := s.CompleteSetup(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !u.IsOperator || u.VerifiedAt == nil {
		t.Fatalf("user: %+v", u)
	}
	info, err := s.Workspace(1).Info(ctx)
	if err != nil || info.Name != "Acme" {
		t.Fatalf("ws1: %v %+v", err, info)
	}
	var role string
	if err := s.db.QueryRowContext(ctx, `SELECT role FROM memberships WHERE user_id = ? AND workspace_id = 1`, u.ID).Scan(&role); err != nil || role != "owner" {
		t.Fatalf("membership: %v %q", err, role)
	}
	if _, err := s.PeekToken(ctx, TokenSetup, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token not consumed: %v", err)
	}
	if _, err := s.CompleteSetup(ctx, in); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second call: %v", err)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("users = %d", n)
	}
}

func TestCompleteSetupReusedTokenCreatesNoUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	raw, _ := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	if _, err := consumeToken(ctx, s.db, TokenSetup, raw, now); err != nil {
		t.Fatal(err)
	}
	_, err := s.CompleteSetup(ctx, SetupInput{Email: "op@example.com", PasswordHash: "phc", WorkspaceName: "Acme", SetupToken: raw, Now: now})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("users = %d", n)
	}
	if info, _ := s.Workspace(1).Info(ctx); info.Name == "Acme" {
		t.Fatal("workspace renamed despite failure")
	}
}

func TestCompleteSetupConcurrent(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	// Two setup tokens can coexist only if inserted directly (CreateToken
	// replaces the previous one).
	raws := []string{"tok-one", "tok-two"}
	for _, r := range raws {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO tokens (token_hash, kind, created_at, expires_at) VALUES (?, 'setup', ?, ?)`,
			hashSecret(r), now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.CompleteSetup(ctx, SetupInput{
				Email: []string{"a@example.com", "b@example.com"}[i], PasswordHash: "phc",
				WorkspaceName: "W", SetupToken: raws[i], Now: now,
			})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrSetupDone) {
			t.Fatal(err)
		}
	}
	if n, _ := s.CountUsers(ctx); n != 1 || ok != 1 {
		t.Fatalf("users = %d, successes = %d", n, ok)
	}
}

func TestIsUpgradedInstall(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	got, err := s.IsUpgradedInstall(ctx)
	if err != nil || got {
		t.Fatalf("fresh: %v %v", got, err)
	}
	if _, err := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Minute, Timeout: time.Second}, now); err != nil {
		t.Fatal(err)
	}
	if got, err = s.IsUpgradedInstall(ctx); err != nil || !got {
		t.Fatalf("with target: %v %v", got, err)
	}
	if _, err := s.CompleteSetup(ctx, SetupInput{Email: "op@example.com", PasswordHash: "phc", WorkspaceName: "W", PasswordUnlocked: true, Now: now}); err != nil {
		t.Fatal(err)
	}
	if got, err = s.IsUpgradedInstall(ctx); err != nil || got {
		t.Fatalf("after setup: %v %v", got, err)
	}
}

func TestNormalizeEmail(t *testing.T) {
	got, err := NormalizeEmail("  A@Example.COM ")
	if err != nil || got != "a@example.com" {
		t.Fatalf("got %q, %v", got, err)
	}
	long := strings.Repeat("a", 255-len("@b.co")) + "@b.co"
	for _, in := range []string{"Bob <a@b.c>", "a@[127.0.0.1]", "a@localhost", "", long, "a@127.0.0.1", "a\u200b@x.co", "a@x\u200d.co", strings.Repeat("Ⱥ", 100) + "@x.co"} {
		if _, err := NormalizeEmail(in); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%q: err = %v", in, err)
		}
	}
}

func TestSetPasswordRevokes(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "old", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateToken(ctx, TokenReset, u.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, u.ID, "new", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekToken(ctx, TokenReset, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset token survived: %v", err)
	}
	_, phc, err := s.UserByEmail(ctx, "a@b.co")
	if err != nil || phc != "new" {
		t.Fatalf("phc = %q, %v", phc, err)
	}
	if err := s.SetPassword(ctx, 9999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	// Sessions are revoked except the one kept.
	keep, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, u.ID, "newer", hashSecret(keep)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, keep, now); err != nil {
		t.Fatalf("kept session: %v", err)
	}
	if _, err := s.SessionAuth(ctx, other, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other session survived: %v", err)
	}
}

func TestCreateAccountAndDuplicate(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, ws, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "h", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID == 0 || ws == nil || ws.ID() == 1 {
		t.Fatalf("u=%+v ws=%v", u, ws)
	}
	w2, info, err := s.UserWorkspace(ctx, u.ID)
	if err != nil || w2.ID() != ws.ID() || info.Name != "A" || info.Slug != "a" {
		t.Fatalf("UserWorkspace: %v %+v", err, info)
	}
	if got, err := s.UserByID(ctx, u.ID); err != nil || got.Email != "a@b.co" {
		t.Fatalf("UserByID: %v %+v", err, got)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "h", WorkspaceName: "B", Slug: "b", Now: now}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	// The failed attempt must not leave a workspace behind.
	if _, err := s.WorkspaceBySlug(ctx, "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan workspace: %v", err)
	}
	if err := s.MarkVerified(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	got, _ := s.UserByID(ctx, u.ID)
	if got.VerifiedAt == nil || !got.VerifiedAt.Equal(now) {
		t.Fatalf("verified: %+v", got.VerifiedAt)
	}
	later := now.Add(time.Hour)
	_ = s.MarkVerified(ctx, u.ID, later)
	got, _ = s.UserByID(ctx, u.ID)
	if !got.VerifiedAt.Equal(now) {
		t.Fatal("MarkVerified overwrote verified_at")
	}
}

func TestPruneTokens(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if _, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	live, _ := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	_ = live
	n, err := s.PruneTokens(ctx, now)
	if err != nil || n != 0 {
		// CreateToken replaced the old one, leaving only the live token.
		t.Fatalf("n=%d err=%v", n, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO tokens (token_hash, kind, created_at, expires_at) VALUES ('old','setup',?,?)`,
		now.Add(-30*24*time.Hour).UnixMilli(), now.Add(-29*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if n, err = s.PruneTokens(ctx, now); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestCompleteSetupExplicitAuthorization(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	in := SetupInput{Email: "op@example.com", PasswordHash: "phc", WorkspaceName: "Acme", Now: now}

	s := open(t)
	if _, err := s.CompleteSetup(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("neither: %v", err)
	}
	in.PasswordUnlocked = true
	if _, err := s.CompleteSetup(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fresh install unlocked: %v", err)
	}
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("users = %d", n)
	}
	if _, err := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Minute, Timeout: time.Second}, now); err != nil {
		t.Fatal(err)
	}
	u, err := s.CompleteSetup(ctx, in)
	if err != nil {
		t.Fatalf("upgraded: %v", err)
	}
	if got, _ := s.UserByID(ctx, u.ID); !got.CreatedAt.Equal(u.CreatedAt) || !got.VerifiedAt.Equal(*u.VerifiedAt) {
		t.Fatalf("times differ: %+v vs %+v", got, u)
	}
}

func TestEmailNormalizedInStore(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 123456789, time.UTC)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "A@X.co", PasswordHash: "h", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil || u.Email != "a@x.co" {
		t.Fatalf("%v %+v", err, u)
	}
	if got, err := s.UserByID(ctx, u.ID); err != nil || got.Email != "a@x.co" || !got.CreatedAt.Equal(u.CreatedAt) {
		t.Fatalf("%v %+v vs %+v", err, got, u)
	}
	if _, _, err := s.UserByEmail(ctx, "A@X.CO"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "h", WorkspaceName: "B", Slug: "b", Now: now}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("dup: %v", err)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "bad", PasswordHash: "h", WorkspaceName: "C", Slug: "c", Now: now}); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("invalid: %v", err)
	}
}

func TestCreateTokenValidation(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	for _, c := range []struct {
		k TokenKind
		u int64
	}{{TokenSetup, 1}, {TokenReset, 0}, {TokenReset, -1}, {"bogus", 0}, {"invite", 1}} {
		if _, err := s.CreateToken(ctx, c.k, c.u, time.Hour, now); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
}

func TestResetTokenScopedPerUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	a, _, _ := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "h", WorkspaceName: "A", Slug: "a", Now: now})
	b, _, _ := s.CreateAccount(ctx, AccountInput{Email: "b@x.co", PasswordHash: "h", WorkspaceName: "B", Slug: "b", Now: now})
	rawB, err := s.CreateToken(ctx, TokenReset, b.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, TokenReset, a.ID, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekToken(ctx, TokenReset, rawB, now); err != nil {
		t.Fatalf("B's token lost: %v", err)
	}
}

func newVerified(t *testing.T, s *Store, now time.Time) User {
	t.Helper()
	ctx := context.Background()
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "s@b.co", PasswordHash: "x", WorkspaceName: "S", Slug: "s", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSessionLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, now)
	ws, _, err := s.UserWorkspace(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, raw).Scan(&n); err != nil || n != 0 {
		t.Fatalf("raw token stored: %d, %v", n, err)
	}
	a, err := s.SessionAuth(ctx, raw, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if a.User.ID != u.ID || a.Workspace.ID != ws.ID() || a.WS.ID() != ws.ID() || a.SessionHash != hashSecret(raw) {
		t.Fatalf("auth = %+v", a)
	}
	if _, err := s.SessionAuth(ctx, raw, now.Add(SessionIdle+time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle: %v", err)
	}

	// Activity keeps a session alive until the absolute expiry.
	raw, _ = s.CreateSession(ctx, u.ID, now)
	day := 24 * time.Hour
	if _, err := s.SessionAuth(ctx, raw, now.Add(6*day)); err != nil {
		t.Fatalf("+6d: %v", err)
	}
	if _, err := s.SessionAuth(ctx, raw, now.Add(12*day)); err != nil {
		t.Fatalf("+12d: %v", err)
	}
	if _, err := s.SessionAuth(ctx, raw, now.Add(SessionTTL+time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absolute expiry: %v", err)
	}

	raw, _ = s.CreateSession(ctx, u.ID, now)
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, now.UnixMilli(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = NULL, verified_at = NULL WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified: %v", err)
	}
}

func TestSessionCapAndRevoke(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, now)
	var raws []string
	for i := 0; i < MaxSessionsPerUser+1; i++ {
		raw, err := s.CreateSession(ctx, u.ID, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, raw)
	}
	at := now.Add(time.Minute)
	if _, err := s.SessionAuth(ctx, raws[0], at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest survived: %v", err)
	}
	for _, r := range raws[1:] {
		if _, err := s.SessionAuth(ctx, r, at); err != nil {
			t.Fatalf("newest 20: %v", err)
		}
	}
	if err := s.DeleteSession(ctx, raws[1]); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, raws[1]); err != nil {
		t.Fatalf("missing session: %v", err)
	}
	if _, err := s.SessionAuth(ctx, raws[1], at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session works: %v", err)
	}
	if err := s.DeleteUserSessions(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	for _, r := range raws[2:] {
		if _, err := s.SessionAuth(ctx, r, at); !errors.Is(err, ErrNotFound) {
			t.Fatalf("after DeleteUserSessions: %v", err)
		}
	}

	// Prune removes expired and idle sessions only.
	live, _ := s.CreateSession(ctx, u.ID, now)
	idle, _ := s.CreateSession(ctx, u.ID, now)
	expired, _ := s.CreateSession(ctx, u.ID, now)
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
		now.Add(-SessionIdle-time.Second).UnixMilli(), hashSecret(idle)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
		now.Add(-time.Second).UnixMilli(), hashSecret(expired)); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneSessions(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	if _, err := s.SessionAuth(ctx, live, now); err != nil {
		t.Fatalf("live pruned: %v", err)
	}
}

func TestSessionAbsoluteExpiryAndIdleBoundary(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, t0)
	day := 24 * time.Hour
	raw, _ := s.CreateSession(ctx, u.ID, t0)
	for _, d := range []time.Duration{6, 12, 18, 24, 29} {
		if _, err := s.SessionAuth(ctx, raw, t0.Add(d*day)); err != nil {
			t.Fatalf("+%dd: %v", d, err)
		}
	}
	if _, err := s.SessionAuth(ctx, raw, t0.Add(SessionTTL-time.Second)); err != nil {
		t.Fatalf("TTL-1s: %v", err)
	}
	if _, err := s.SessionAuth(ctx, raw, t0.Add(SessionTTL)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("at TTL: %v", err)
	}

	raw, _ = s.CreateSession(ctx, u.ID, t0)
	if _, err := s.SessionAuth(ctx, raw, t0.Add(SessionIdle)); err != nil {
		t.Fatalf("exactly idle: %v", err)
	}
	raw, _ = s.CreateSession(ctx, u.ID, t0)
	if _, err := s.SessionAuth(ctx, raw, t0.Add(SessionIdle+time.Millisecond)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle+1ms: %v", err)
	}
}

func TestSessionCapIgnoresDeadSessions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, now)
	var raws []string
	for i := 0; i < MaxSessionsPerUser; i++ {
		raw, err := s.CreateSession(ctx, u.ID, now.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, raw)
	}
	for _, r := range raws[:5] {
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?`,
			now.Add(-SessionIdle-time.Hour).UnixMilli(), hashSecret(r)); err != nil {
			t.Fatal(err)
		}
	}
	later := now.Add(time.Minute)
	nw, err := s.CreateSession(ctx, u.ID, later)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range append(raws[5:], nw) {
		if _, err := s.SessionAuth(ctx, r, later); err != nil {
			t.Fatalf("live session evicted: %v", err)
		}
	}
	var n int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, u.ID).Scan(&n)
	if n != MaxSessionsPerUser-5+1 {
		t.Fatalf("count = %d", n)
	}
}

func TestSessionTouchThrottle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, t0)
	raw, _ := s.CreateSession(ctx, u.ID, t0)
	seen := func() (sess int64, user *int64) {
		t.Helper()
		if err := s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM sessions WHERE token_hash = ?`, hashSecret(raw)).Scan(&sess); err != nil {
			t.Fatal(err)
		}
		var v sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT last_seen_at FROM users WHERE id = ?`, u.ID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v.Valid {
			user = &v.Int64
		}
		return
	}
	if _, err := s.SessionAuth(ctx, raw, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ss, us := seen(); ss != t0.UnixMilli() || us != nil {
		t.Fatalf("touched early: %d %v", ss, us)
	}
	t5 := t0.Add(5 * time.Minute)
	if _, err := s.SessionAuth(ctx, raw, t5); err != nil {
		t.Fatal(err)
	}
	if ss, us := seen(); ss != t5.UnixMilli() || us == nil || *us != t5.UnixMilli() {
		t.Fatalf("not touched: %d %v", ss, us)
	}
}

func TestCompleteSetupConcurrentTokenAndPassword(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if _, err := s.Workspace(1).CreateTarget(ctx, Target{Name: "A", URL: "https://a", Interval: time.Minute, Timeout: time.Second}, now); err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	ins := []SetupInput{
		{Email: "a@example.com", PasswordHash: "phc", WorkspaceName: "W", SetupToken: raw, Now: now},
		{Email: "b@example.com", PasswordHash: "phc", WorkspaceName: "W", PasswordUnlocked: true, Now: now},
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range ins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = s.CompleteSetup(ctx, ins[i])
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrSetupDone) {
			t.Fatal(err)
		}
	}
	if n, _ := s.CountUsers(ctx); n != 1 || ok != 1 {
		t.Fatalf("users = %d, successes = %d, errs = %v", n, ok, errs)
	}
}

func TestResetPassword(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "old", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkVerified(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	sess, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateToken(ctx, TokenReset, u.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, "bogus", "x", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bogus token: %v", err)
	}
	if _, _, err := s.UserByEmail(ctx, "a@b.co"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ResetPassword(ctx, raw, "new", now)
	if err != nil || got.ID != u.ID || got.VerifiedAt == nil {
		t.Fatalf("ResetPassword = %+v, %v", got, err)
	}
	if _, phc, _ := s.UserByEmail(ctx, "a@b.co"); phc != "new" {
		t.Errorf("phc = %q", phc)
	}
	if _, err := s.SessionAuth(ctx, sess, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("session survived: %v", err)
	}
	if _, err := s.ResetPassword(ctx, raw, "again", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("token reuse: %v", err)
	}
	if _, phc, _ := s.UserByEmail(ctx, "a@b.co"); phc != "new" {
		t.Errorf("reused token changed password: %q", phc)
	}
}
