package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
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
	if err := markVerified(s, u.ID, now); err != nil {
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
	w2, info, err := userWorkspace(s, u.ID)
	if err != nil || w2.ID() != ws.ID() || info.Name != "A" || info.Slug != "a" {
		t.Fatalf("UserWorkspace: %v %+v", err, info)
	}
	if got, err := userByID(s, u.ID); err != nil || got.Email != "a@b.co" {
		t.Fatalf("UserByID: %v %+v", err, got)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "h", WorkspaceName: "B", Slug: "b", Now: now}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate: %v", err)
	}
	// The failed attempt must not leave a workspace behind.
	if n := slugRows(t, s, "b"); n != 0 {
		t.Fatalf("orphan workspace: %d rows", n)
	}
	if err := markVerified(s, u.ID, now); err != nil {
		t.Fatal(err)
	}
	got, _ := userByID(s, u.ID)
	if got.VerifiedAt == nil || !got.VerifiedAt.Equal(now) {
		t.Fatalf("verified: %+v", got.VerifiedAt)
	}
	later := now.Add(time.Hour)
	_ = markVerified(s, u.ID, later)
	got, _ = userByID(s, u.ID)
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
	if got, _ := userByID(s, u.ID); !got.CreatedAt.Equal(u.CreatedAt) || !got.VerifiedAt.Equal(*u.VerifiedAt) {
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
	if got, err := userByID(s, u.ID); err != nil || got.Email != "a@x.co" || !got.CreatedAt.Equal(u.CreatedAt) {
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
	if err := markVerified(s, u.ID, now); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestSessionLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, now)
	ws, _, err := userWorkspace(s, u.ID)
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
	if u.VerifiedAt != nil {
		t.Fatal("account should start unverified")
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
	if again, _ := userByID(s, u.ID); again.VerifiedAt == nil {
		t.Error("verified_at not stored")
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

func TestResetPasswordDisabledUser(t *testing.T) {
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
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, now.UnixMilli(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, raw, "new", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled user: %v", err)
	}
	if _, phc, _ := s.UserByEmail(ctx, "a@b.co"); phc != "old" {
		t.Errorf("password changed: %q", phc)
	}
}

// Test helpers: SQL stand-ins for store methods the application does not need.

func markVerified(s *Store, userID int64, now time.Time) error {
	_, err := s.db.Exec(`UPDATE users SET verified_at = ? WHERE id = ? AND verified_at IS NULL`, now.UnixMilli(), userID)
	return err
}

func userByID(s *Store, id int64) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func userWorkspace(s *Store, userID int64) (*WS, Workspace, error) {
	ws, err := scanWorkspace(s.db.QueryRow(
		`SELECT w.id, w.name, w.slug, w.created_at FROM workspaces w
		 JOIN memberships m ON m.workspace_id = w.id
		 WHERE m.user_id = ? AND m.role = 'owner' ORDER BY w.id LIMIT 1`, userID))
	if err != nil {
		return nil, Workspace{}, err
	}
	return s.Workspace(ws.ID), ws, nil
}

func TestInviteTokens(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ttl := 7 * 24 * time.Hour

	bound, err := s.CreateInvite(ctx, "  New@X.Co ", ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	open1, err := s.CreateInvite(ctx, "", ttl, now)
	if err != nil {
		t.Fatal(err)
	}
	// Creating the second invite deleted nothing.
	tok, err := s.PeekToken(ctx, TokenInvite, bound, now)
	if err != nil || tok.Email != "new@x.co" || tok.UserID != 0 || tok.Kind != TokenInvite {
		t.Fatalf("bound: %v %+v", err, tok)
	}
	tok, err = s.PeekToken(ctx, TokenInvite, open1, now)
	if err != nil || tok.Email != "" {
		t.Fatalf("unbound: %v %+v", err, tok)
	}
	if _, err := s.CreateInvite(ctx, "not an email", ttl, now); err == nil {
		t.Fatal("invalid email accepted")
	}
	// Expiry after 7 days.
	if _, err := s.PeekToken(ctx, TokenInvite, bound, now.Add(ttl-time.Second)); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	if _, err := s.PeekToken(ctx, TokenInvite, bound, now.Add(ttl+time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after expiry: %v", err)
	}
	// Single use.
	if _, err := consumeToken(ctx, s.db, TokenInvite, open1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeToken(ctx, s.db, TokenInvite, open1, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second consume: %v", err)
	}
	if _, err := s.PeekToken(ctx, TokenInvite, bound, now); err != nil {
		t.Fatalf("other invite lost: %v", err)
	}
}

func TestCreateAccountWithInvite(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	ttl := 7 * 24 * time.Hour
	userCount := func() int { n, _ := s.CountUsers(ctx); return n }

	inv, _ := s.CreateInvite(ctx, "", ttl, now)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", InviteToken: inv, Verified: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if u.VerifiedAt == nil {
		t.Fatal("Verified: true did not set VerifiedAt")
	}
	got, _ := userByID(s, u.ID)
	if got.VerifiedAt == nil {
		t.Fatal("verified_at not stored")
	}
	if _, err := s.PeekToken(ctx, TokenInvite, inv, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invite not consumed: %v", err)
	}

	// Without Verified the user is unverified.
	u2, _, err := s.CreateAccount(ctx, AccountInput{Email: "u@x.co", PasswordHash: "x", WorkspaceName: "U", Slug: "u", Now: now})
	if err != nil || u2.VerifiedAt != nil {
		t.Fatalf("unverified: %v %+v", err, u2)
	}

	// Duplicate email: invite stays unused and a retry succeeds.
	inv2, _ := s.CreateInvite(ctx, "", ttl, now)
	_, _, err = s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "x", WorkspaceName: "A2", Slug: "a2", InviteToken: inv2, Now: now})
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("dup: %v", err)
	}
	if _, err := s.PeekToken(ctx, TokenInvite, inv2, now); err != nil {
		t.Fatalf("invite consumed by failed create: %v", err)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "c@x.co", PasswordHash: "x", WorkspaceName: "C", Slug: "c", InviteToken: inv2, Now: now}); err != nil {
		t.Fatalf("retry: %v", err)
	}

	// Email mismatch writes nothing and leaves the invite unused.
	before := userCount()
	inv3, _ := s.CreateInvite(ctx, "a@x.co", ttl, now)
	_, _, err = s.CreateAccount(ctx, AccountInput{Email: "b@x.co", PasswordHash: "x", WorkspaceName: "B", Slug: "b", InviteToken: inv3, Now: now})
	if !errors.Is(err, ErrInviteEmail) {
		t.Fatalf("mismatch: %v", err)
	}
	if userCount() != before {
		t.Fatal("mismatch wrote a user")
	}
	if n := slugRows(t, s, "b"); n != 0 {
		t.Fatalf("mismatch wrote a workspace: %d rows", n)
	}
	if _, err := s.PeekToken(ctx, TokenInvite, inv3, now); err != nil {
		t.Fatalf("invite consumed by mismatch: %v", err)
	}
	// The bound email matches case-insensitively, so the mismatch check
	// passes and the already-registered address is what fails.
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "A@X.co", PasswordHash: "x", WorkspaceName: "A3", Slug: "a3", InviteToken: inv3, Now: now}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("matching email on taken address: %v", err)
	}

	// Unknown invite creates nothing.
	before = userCount()
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "z@x.co", PasswordHash: "x", WorkspaceName: "Z", Slug: "z", InviteToken: "bogus", Now: now}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bogus invite: %v", err)
	}
	if userCount() != before {
		t.Fatal("bogus invite wrote a user")
	}

	// Concurrent use of one invite creates exactly one user.
	inv4, _ := s.CreateInvite(ctx, "", ttl, now)
	before = userCount()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := []string{"p@x.co", "q@x.co"}[i]
			_, _, errs[i] = s.CreateAccount(ctx, AccountInput{Email: e, PasswordHash: "x", WorkspaceName: "W" + e, Slug: "w" + e[:1], InviteToken: inv4, Now: now})
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else if !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if ok != 1 || userCount() != before+1 {
		t.Fatalf("successes = %d, users added = %d", ok, userCount()-before)
	}
}

func TestDeriveSlug(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

	got, err := DeriveSlug(ctx, s.db, "My Team!")
	if err != nil || got != "my-team" {
		t.Fatalf("my team: %q %v", got, err)
	}
	for _, name := range []string{"  ---  ", "é", "ab"} {
		got, err := DeriveSlug(ctx, s.db, name)
		if err != nil || !strings.HasPrefix(got, "ws-") || len(got) != 9 || !ValidSlug(got) {
			t.Fatalf("%q: %q %v", name, got, err)
		}
		for _, r := range got[3:] {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz234567", r) {
				t.Fatalf("%q: %q not base32", name, got)
			}
		}
	}
	got, err = DeriveSlug(ctx, s.db, strings.Repeat("abcdefghi ", 6))
	if err != nil || len(got) > 40 || !ValidSlug(got) {
		t.Fatalf("long: %q %v", got, err)
	}

	if _, err := createWorkspace(ctx, s.db, "My Team", "my-team", now); err != nil {
		t.Fatal(err)
	}
	got, _ = DeriveSlug(ctx, s.db, "My Team!")
	if got != "my-team-2" {
		t.Fatalf("collision: %q", got)
	}
	if _, err := createWorkspace(ctx, s.db, "My Team", got, now); err != nil {
		t.Fatal(err)
	}
	got, _ = DeriveSlug(ctx, s.db, "My Team!")
	if got != "my-team-3" {
		t.Fatalf("second collision: %q", got)
	}

	long := strings.Repeat("a", 40)
	if _, err := createWorkspace(ctx, s.db, "L", long, now); err != nil {
		t.Fatal(err)
	}
	got, _ = DeriveSlug(ctx, s.db, long)
	if got != strings.Repeat("a", 38)+"-2" || len(got) != 40 {
		t.Fatalf("long collision: %q", got)
	}

	// CreateAccount derives a slug when none is given.
	_, ws, err := s.CreateAccount(ctx, AccountInput{Email: "d@x.co", PasswordHash: "x", WorkspaceName: "My Team", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := ws.Info(ctx)
	if info.Slug != "my-team-3" {
		t.Fatalf("derived slug: %q", info.Slug)
	}
}

func TestValidSlug(t *testing.T) {
	for s, want := range map[string]bool{
		"abc": true, "a-b": true, "my-team-2": true, strings.Repeat("a", 40): true,
		"ab": false, "": false, "-ab": false, "ab-": false, "Abc": false, "a_b": false, "a b": false,
		strings.Repeat("a", 41): false, "é-é": false,
	} {
		if ValidSlug(s) != want {
			t.Errorf("ValidSlug(%q) = %v", s, !want)
		}
	}
}

func TestSetSlugAndRename(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, a, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "x", WorkspaceName: "A", Slug: "alpha", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "b@x.co", PasswordHash: "x", WorkspaceName: "B", Slug: "beta", Now: now}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "ab", "-abc", "abc-", "ABC", "a_c", strings.Repeat("a", 41)} {
		if err := a.SetSlug(ctx, bad); !errors.Is(err, ErrInvalidSlug) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if err := a.SetSlug(ctx, "beta"); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("taken: %v", err)
	}
	if err := a.SetSlug(ctx, "gamma"); err != nil {
		t.Fatal(err)
	}
	if err := a.SetSlug(ctx, "gamma"); err != nil {
		t.Fatalf("same slug on itself: %v", err)
	}
	if err := a.Rename(ctx, "Renamed"); err != nil {
		t.Fatal(err)
	}
	info, _ := a.Info(ctx)
	if info.Slug != "gamma" || info.Name != "Renamed" {
		t.Fatalf("info: %+v", info)
	}
	if n := slugRows(t, s, "alpha"); n != 0 {
		t.Fatalf("old slug: %d rows", n)
	}
}

func TestCounts(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if OperatorWorkspaceID != 1 {
		t.Fatal("operator workspace is not 1")
	}
	if n, err := s.CountWorkspaces(ctx); err != nil || n != 1 {
		t.Fatalf("workspaces: %d %v", n, err)
	}
	_, a, _ := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "x", WorkspaceName: "A", Slug: "alpha", Now: now})
	_, b, _ := s.CreateAccount(ctx, AccountInput{Email: "b@x.co", PasswordHash: "x", WorkspaceName: "B", Slug: "beta", Now: now})
	if n, _ := s.CountWorkspaces(ctx); n != 3 {
		t.Fatalf("workspaces = %d", n)
	}
	mk := func(w *WS, name string) {
		t.Helper()
		if _, err := w.CreateTarget(ctx, Target{Name: name, URL: "https://" + name, Interval: time.Minute, Timeout: time.Second}, now); err != nil {
			t.Fatal(err)
		}
	}
	mk(s.Workspace(1), "op1")
	mk(a, "a1")
	mk(a, "a2")
	mk(b, "b1")
	if n, err := a.CountTargets(ctx); err != nil || n != 2 {
		t.Fatalf("a targets: %d %v", n, err)
	}
	if n, _ := s.Workspace(1).CountTargets(ctx); n != 1 {
		t.Fatalf("op targets: %d", n)
	}
	others, err := s.TargetsInOtherWorkspaces(ctx, a.ID())
	if err != nil || len(others) != 1 || others[0].URL != "https://b1" {
		t.Fatalf("others: %v %+v", err, others)
	}
	others, _ = s.TargetsInOtherWorkspaces(ctx, 999)
	if len(others) != 3 {
		t.Fatalf("others of none = %d", len(others))
	}
	if o := others[0]; o.URL == "" || o.Interval != time.Minute || o.ID == 0 || o.WorkspaceID == 0 || o.Name != "" {
		t.Fatalf("fields = %+v", o)
	}
	// A second operator's workspace is excluded as well.
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET is_operator = 1 WHERE email = 'b@x.co'`); err != nil {
		t.Fatal(err)
	}
	others, _ = s.TargetsInOtherWorkspaces(ctx, a.ID())
	if len(others) != 0 {
		t.Fatalf("operator-owned workspace counted: %+v", others)
	}
}

func TestCreateTargetCapped(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, w, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "x", WorkspaceName: "A", Slug: "alpha", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string, max int) error {
		_, err := w.CreateTargetCapped(ctx, Target{Name: name, URL: "https://" + name, Interval: time.Minute, Timeout: time.Second}, now, max)
		return err
	}
	if err := mk("first", 3); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = mk("c"+strconv.Itoa(i), 3)
		}()
	}
	wg.Wait()
	ok, capped := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case errors.Is(e, ErrMonitorCap):
			capped++
		default:
			t.Fatal(e)
		}
	}
	if n, _ := w.CountTargets(ctx); n != 3 || ok != 2 || capped != 2 {
		t.Fatalf("count %d ok %d capped %d", n, ok, capped)
	}
	if err := mk("unlimited", 0); err != nil {
		t.Fatalf("max 0 should be unlimited: %v", err)
	}
}

func TestDeleteAccount(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	op, err := s.CompleteSetup(ctx, func() SetupInput {
		raw, _ := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
		return SetupInput{Email: "op@x.co", PasswordHash: "x", WorkspaceName: "Op", SetupToken: raw, Now: now}
	}())
	if err != nil {
		t.Fatal(err)
	}
	opWS := s.Workspace(1)
	seedWS(t, s, opWS, "opsite", now)

	ub, wsB, err := s.CreateAccount(ctx, AccountInput{Email: "b@x.co", PasswordHash: "x", WorkspaceName: "B", Slug: "beta", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	fx := seedWS(t, s, wsB, "bsite", now)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO daily (target_id, day, total, ok, lat_sum) VALUES (?, 1, 1, 1, 1)`, fx.target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, ub.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateToken(ctx, TokenReset, ub.ID, time.Hour, now); err != nil {
		t.Fatal(err)
	}
	// A workspace shared with another member survives.
	uc, wsC, _ := s.CreateAccount(ctx, AccountInput{Email: "c@x.co", PasswordHash: "x", WorkspaceName: "C", Slug: "gamma", Now: now})
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memberships (user_id, workspace_id, role, created_at) VALUES (?, ?, 'viewer', ?)`, ub.ID, wsC.ID(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = uc

	seedWS(t, s, wsC, "csite", now)
	opBefore, cBefore := countsOf(t, opWS, now), countsOf(t, wsC, now)

	deleted, err := s.DeleteAccount(ctx, ub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != wsB.ID() {
		t.Fatalf("deleted = %v", deleted)
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tbl := range []string{"checks", "incidents", "alerts", "daily"} {
		if n := count(`SELECT COUNT(*) FROM `+tbl+` WHERE target_id = ?`, fx.target.ID); n != 0 {
			t.Errorf("%s rows left: %d", tbl, n)
		}
	}
	for q, args := range map[string][]any{
		`SELECT COUNT(*) FROM workspaces WHERE id = ?`:          {wsB.ID()},
		`SELECT COUNT(*) FROM targets WHERE workspace_id=?`:     {wsB.ID()},
		`SELECT COUNT(*) FROM maintenance WHERE workspace_id=?`: {wsB.ID()},
		`SELECT COUNT(*) FROM settings WHERE workspace_id=?`:    {wsB.ID()},
		`SELECT COUNT(*) FROM users WHERE id = ?`:               {ub.ID},
		`SELECT COUNT(*) FROM sessions WHERE user_id = ?`:       {ub.ID},
		`SELECT COUNT(*) FROM tokens WHERE user_id = ?`:         {ub.ID},
		`SELECT COUNT(*) FROM memberships WHERE user_id = ?`:    {ub.ID},
	} {
		if n := count(q, args...); n != 0 {
			t.Errorf("%s = %d", q, n)
		}
	}
	if n := count(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, wsC.ID()); n != 1 {
		t.Error("shared workspace deleted")
	}
	if _, err := s.DeleteAccount(ctx, ub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	// The only operator cannot be deleted and nothing changes.
	if _, err := s.DeleteAccount(ctx, op.ID); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("operator: %v", err)
	}
	if after := countsOf(t, opWS, now); after != opBefore {
		t.Fatalf("operator data changed: %+v -> %+v", opBefore, after)
	}
	if after := countsOf(t, wsC, now); after != cBefore {
		t.Fatalf("other workspace changed: %+v -> %+v", cBefore, after)
	}
	if n := count(`SELECT COUNT(*) FROM users WHERE id = ?`, op.ID); n != 1 {
		t.Fatal("operator deleted")
	}
}

func TestDeleteAccountOperatorGuards(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	raw, _ := s.CreateToken(ctx, TokenSetup, 0, time.Hour, now)
	op1, err := s.CompleteSetup(ctx, SetupInput{Email: "op1@x.co", PasswordHash: "x", WorkspaceName: "Op", SetupToken: raw, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	opWS := s.Workspace(OperatorWorkspaceID)
	seedWS(t, s, opWS, "opsite", now)
	op2, _, err := s.CreateAccount(ctx, AccountInput{Email: "op2@x.co", PasswordHash: "x", WorkspaceName: "Op2", Slug: "op2", Operator: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	// op2 becomes the sole owner of workspace 1.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memberships WHERE user_id = ?`, op1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO memberships (user_id, workspace_id, role, created_at) VALUES (?, 1, 'owner', ?)`, op2.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// A disabled second operator does not count.
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, now.UnixMilli(), op1.ID); err != nil {
		t.Fatal(err)
	}
	before := countsOf(t, opWS, now)
	if _, err := s.DeleteAccount(ctx, op2.ID); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("only active operator: %v", err)
	}
	// With op1 active again, op2 can go, but workspace 1 stays.
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = NULL WHERE id = ?`, op1.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.DeleteAccount(ctx, op2.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range deleted {
		if id == OperatorWorkspaceID {
			t.Fatal("operator workspace reported deleted")
		}
	}
	if _, err := opWS.Info(ctx); err != nil {
		t.Fatalf("workspace 1: %v", err)
	}
	if after := countsOf(t, opWS, now); after != before {
		t.Fatalf("workspace 1 data changed: %+v -> %+v", before, after)
	}
}

func TestExplicitSlugCollision(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	_, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@x.co", PasswordHash: "x", WorkspaceName: "A", Slug: "default", Now: now})
	if !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("err = %v", err)
	}
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("users = %d", n)
	}
}

func TestDeriveSlugFallbackAfterManyCollisions(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	slugs := []string{"xyz"}
	for i := 2; i <= 21; i++ {
		slugs = append(slugs, "xyz-"+strconv.Itoa(i))
	}
	for _, sl := range slugs {
		if _, err := createWorkspace(ctx, s.db, "X", sl, now); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DeriveSlug(ctx, s.db, "xyz")
	if err != nil || !ValidSlug(got) || !strings.HasPrefix(got, "xyz-") || len(got) != 8 {
		t.Fatalf("got %q %v", got, err)
	}
	if n := slugRows(t, s, got); n != 0 {
		t.Fatalf("not unique: %d rows", n)
	}
}

func TestCreateAccountWorkspaceCap(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	// The operator workspace exists, so a cap of 2 leaves room for one more.
	n, err := s.CountWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	max := n + 1
	errs := make(chan error, 2)
	for _, e := range []string{"a@b.co", "c@d.co"} {
		go func() {
			_, _, err := s.CreateAccount(ctx, AccountInput{Email: e, PasswordHash: "h", WorkspaceName: e, MaxWorkspaces: max, Now: now})
			errs <- err
		}()
	}
	var ok, capped int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			ok++
		case errors.Is(err, ErrWorkspaceCap):
			capped++
		default:
			t.Fatal(err)
		}
	}
	if ok != 1 || capped != 1 {
		t.Fatalf("ok=%d capped=%d, want 1 and 1", ok, capped)
	}
	if got, _ := s.CountWorkspaces(ctx); got != max {
		t.Fatalf("workspaces = %d, want %d", got, max)
	}
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "e@f.co", PasswordHash: "h", WorkspaceName: "E", Now: now}); err != nil {
		t.Fatalf("MaxWorkspaces 0 is unlimited: %v", err)
	}
}

func TestVerifyToken(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateToken(ctx, TokenVerify, u.ID, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.VerifyEmail(ctx, raw, now.Add(time.Minute))
	if err != nil || got.ID != u.ID || got.VerifiedAt == nil {
		t.Fatalf("verify: %+v %v", got, err)
	}
	if _, err := s.VerifyEmail(ctx, raw, now.Add(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reuse: %v", err)
	}
	raw2, _ := s.CreateToken(ctx, TokenVerify, u.ID, 24*time.Hour, now)
	if _, err := s.VerifyEmail(ctx, raw2, now.Add(25*time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	reset, _ := s.CreateToken(ctx, TokenReset, u.ID, time.Hour, now)
	if _, err := s.VerifyEmail(ctx, reset, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reset token: %v", err)
	}
	if _, err := s.CreateToken(ctx, TokenVerify, 0, time.Hour, now); err == nil {
		t.Fatal("verify token without a user accepted")
	}
}

func TestVerifyTokenDisabledUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CreateToken(ctx, TokenVerify, u.ID, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, now.UnixMilli(), u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PeekToken(ctx, TokenVerify, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("peek: %v", err)
	}
	if _, err := s.VerifyEmail(ctx, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verify: %v", err)
	}
	if got, _ := userByID(s, u.ID); got.VerifiedAt != nil {
		t.Fatal("disabled user was verified")
	}
}

func TestNewVerifyTokenReplacesOld(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, _ := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: now})
	first, _ := s.CreateToken(ctx, TokenVerify, u.ID, time.Hour, now)
	second, _ := s.CreateToken(ctx, TokenVerify, u.ID, time.Hour, now)
	if _, err := s.VerifyEmail(ctx, first, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old token: %v", err)
	}
	if _, err := s.VerifyEmail(ctx, second, now); err != nil {
		t.Fatalf("new token: %v", err)
	}
}

func TestSetPasswordDeletesVerifyTokens(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, _ := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: now})
	raw, _ := s.CreateToken(ctx, TokenVerify, u.ID, time.Hour, now)
	if err := s.SetPassword(ctx, u.ID, "new", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyEmail(ctx, raw, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("verify after password change: %v", err)
	}
}

func TestPruneUnverified(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-49 * time.Hour)
	stale, wsStale, err := s.CreateAccount(ctx, AccountInput{Email: "stale@b.co", PasswordHash: "x", WorkspaceName: "Stale", Slug: "stale", Now: old})
	if err != nil {
		t.Fatal(err)
	}
	fx := seedWS(t, s, wsStale, "stale", old)
	if _, err := s.CreateToken(ctx, TokenVerify, stale.ID, 24*time.Hour, old); err != nil {
		t.Fatal(err)
	}
	fresh, wsFresh, _ := s.CreateAccount(ctx, AccountInput{Email: "fresh@b.co", PasswordHash: "x", WorkspaceName: "Fresh", Slug: "fresh", Now: now.Add(-time.Hour)})
	verified, wsVerified, _ := s.CreateAccount(ctx, AccountInput{Email: "ver@b.co", PasswordHash: "x", WorkspaceName: "Ver", Slug: "ver", Now: old})
	if err := markVerified(s, verified.ID, old); err != nil {
		t.Fatal(err)
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO users (email, password_hash, is_operator, created_at) VALUES ('op@b.co', 'x', 1, ?)`, old.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	opID, _ := res.LastInsertId()

	ids, err := s.PruneUnverified(ctx, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != wsStale.ID() {
		t.Fatalf("deleted workspaces %v, want [%d]", ids, wsStale.ID())
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count(`SELECT COUNT(*) FROM users WHERE id = ?`, stale.ID) != 0 {
		t.Error("stale user survived")
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM workspaces WHERE id = ?`,
		`SELECT COUNT(*) FROM memberships WHERE workspace_id = ?`,
		`SELECT COUNT(*) FROM targets WHERE workspace_id = ?`,
	} {
		if n := count(q, wsStale.ID()); n != 0 {
			t.Errorf("%s: %d rows left", q, n)
		}
	}
	if n := count(`SELECT COUNT(*) FROM checks WHERE target_id = ?`, fx.target.ID); n != 0 {
		t.Errorf("checks left: %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM tokens WHERE user_id = ?`, stale.ID); n != 0 {
		t.Errorf("tokens left: %d", n)
	}
	for _, id := range []int64{fresh.ID, verified.ID, opID} {
		if count(`SELECT COUNT(*) FROM users WHERE id = ?`, id) != 1 {
			t.Errorf("user %d was deleted", id)
		}
	}
	for _, w := range []*WS{wsFresh, wsVerified} {
		if count(`SELECT COUNT(*) FROM workspaces WHERE id = ?`, w.ID()) != 1 {
			t.Errorf("workspace %d was deleted", w.ID())
		}
	}
}

func TestPruneUnverifiedRechecksInTransaction(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	old := now.Add(-49 * time.Hour)
	u, _, _ := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: old})
	raw, _ := s.CreateToken(ctx, TokenVerify, u.ID, 72*time.Hour, old)
	if _, err := s.VerifyEmail(ctx, raw, now); err != nil {
		t.Fatal(err)
	}
	ids, err := s.PruneUnverified(ctx, now.Add(-48*time.Hour))
	if err != nil || len(ids) != 0 {
		t.Fatalf("prune verified: %v %v", ids, err)
	}
	if _, err := userByID(s, u.ID); err != nil {
		t.Fatalf("verified user gone: %v", err)
	}
}

func TestCreateAccountCapBeforeDuplicate(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "a", Now: now}); err != nil {
		t.Fatal(err)
	}
	n, err := s.CountWorkspaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A2", MaxWorkspaces: n, Now: now})
	if !errors.Is(err, ErrWorkspaceCap) {
		t.Fatalf("duplicate at cap: %v", err)
	}
}

func TestWorkspaceBySlugUnverified(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u, _, _ := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "alpha", Now: now})
	if _, err := s.WorkspaceBySlug(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified owner: %v", err)
	}
	raw, _ := s.CreateToken(ctx, TokenVerify, u.ID, time.Hour, now)
	if _, err := s.VerifyEmail(ctx, raw, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkspaceBySlug(ctx, "alpha"); err != nil {
		t.Fatalf("verified owner: %v", err)
	}
	if _, err := s.WorkspaceBySlug(ctx, "default"); err != nil {
		t.Fatalf("workspace 1: %v", err)
	}
}

// slugRows counts workspaces with a slug straight from the table.
// WorkspaceBySlug also hides workspaces without a verified owner, so it
// cannot prove that a workspace is absent.
func slugRows(t *testing.T, s *Store, slug string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE slug = ?`, slug).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
