package store

import (
	"context"
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
	// Session revocation (keepSessionHash) is covered with Task 4's sessions.
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
