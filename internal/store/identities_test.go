package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func mkUser(t *testing.T, s *Store, email, phc string) User {
	t.Helper()
	u, _, err := s.CreateAccount(context.Background(), AccountInput{Email: email, PasswordHash: phc, WorkspaceName: "W" + email, Slug: "w" + email[:1], Verified: true, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestIdentityLinkLookupAndUniqueness(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	a, b := mkUser(t, s, "a@x.co", "h"), mkUser(t, s, "b@x.co", "h")
	if _, err := s.IdentityUser(ctx, "google", "g1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := s.LinkIdentity(ctx, a.ID, "google", "g1", "a@gmail.com", now); err != nil {
		t.Fatal(err)
	}
	u, err := s.IdentityUser(ctx, "google", "g1")
	if err != nil || u.ID != a.ID || u.Email != "a@x.co" {
		t.Fatalf("IdentityUser = %+v, %v", u, err)
	}
	// (provider, subject) is linked to another user.
	if err := s.LinkIdentity(ctx, b.ID, "google", "g1", "a@gmail.com", now); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("err = %v, want ErrIdentityTaken", err)
	}
	// The user already has this provider.
	if err := s.LinkIdentity(ctx, a.ID, "google", "g2", "a2@gmail.com", now); !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("err = %v, want ErrIdentityTaken", err)
	}
	// Same subject string under another provider is distinct.
	if err := s.LinkIdentity(ctx, b.ID, "github", "g1", "b@x.co", now); err != nil {
		t.Fatal(err)
	}
	ids, err := s.Identities(ctx, a.ID)
	if err != nil || len(ids) != 1 || ids[0].Provider != "google" || ids[0].Email != "a@gmail.com" || ids[0].UserID != a.ID || ids[0].LastUsedAt != nil {
		t.Fatalf("Identities = %+v, %v", ids, err)
	}
	if err := s.LinkIdentity(ctx, a.ID, "gitlab", "x", "a@x.co", now); err == nil {
		t.Fatal("unknown provider should fail")
	}
	if err := s.LinkIdentity(ctx, 999, "github", "z", "a@x.co", now); err == nil {
		t.Fatal("unknown user should fail")
	}
}

func TestTouchIdentity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	a := mkUser(t, s, "a@x.co", "h")
	if err := s.LinkIdentity(ctx, a.ID, "github", "7", "a@x.co", now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(time.Hour)
	if err := s.TouchIdentity(ctx, "github", "7", later); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.Identities(ctx, a.ID)
	if len(ids) != 1 || ids[0].LastUsedAt == nil || !ids[0].LastUsedAt.Equal(later) || !ids[0].CreatedAt.Equal(now) {
		t.Fatalf("Identities = %+v", ids)
	}
	if err := s.TouchIdentity(ctx, "github", "nope", later); err != nil {
		t.Fatalf("touching a missing identity: %v", err)
	}
}

func TestIdentitiesCascadeOnUserDelete(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	a := mkUser(t, s, "a@x.co", "h")
	if err := s.LinkIdentity(ctx, a.ID, "google", "g1", "a@x.co", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteAccount(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM identities`); n != 0 {
		t.Fatalf("identities = %d, want 0", n)
	}
}

func TestUnlinkIdentityLastMethod(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	// Social-only user: the only identity cannot go.
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "s@x.co", WorkspaceName: "S", Slug: "s", Verified: true, Now: now,
		Identity: &NewIdentity{Provider: "google", Subject: "g1", Email: "s@gmail.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UnlinkIdentity(ctx, u.ID, "google"); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("err = %v, want ErrLastSignInMethod", err)
	}
	if err := s.LinkIdentity(ctx, u.ID, "github", "7", "s@x.co", now); err != nil {
		t.Fatal(err)
	}
	if err := s.UnlinkIdentity(ctx, u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if err := s.UnlinkIdentity(ctx, u.ID, "github"); !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("err = %v, want ErrLastSignInMethod", err)
	}
	if err := s.UnlinkIdentity(ctx, u.ID, "google"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// With a password, the last identity may go.
	p := mkUser(t, s, "p@x.co", "hash")
	if err := s.LinkIdentity(ctx, p.ID, "google", "g9", "p@x.co", now); err != nil {
		t.Fatal(err)
	}
	if err := s.UnlinkIdentity(ctx, p.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.Identities(ctx, p.ID); len(ids) != 0 {
		t.Fatalf("identities = %+v", ids)
	}
}

func TestHasPasswordAndSetFirstPassword(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "s@x.co", WorkspaceName: "S", Slug: "s", Verified: true, Now: now,
		Identity: &NewIdentity{Provider: "github", Subject: "7", Email: "s@x.co"}})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.HasPassword(ctx, u.ID); err != nil || ok {
		t.Fatalf("HasPassword = %v, %v; want false", ok, err)
	}
	if _, err := s.HasPassword(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	keep, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession(ctx, u.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFirstPassword(ctx, u.ID, "phc1", hashSecret(keep)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasPassword(ctx, u.ID); !ok {
		t.Fatal("HasPassword should be true")
	}
	if n := rawCount(t, s.db, fmt.Sprintf(`SELECT COUNT(*) FROM sessions WHERE user_id = %d`, u.ID)); n != 1 {
		t.Fatalf("sessions = %d, want 1 (the kept one)", n)
	}
	// A second call must not overwrite.
	if err := s.SetFirstPassword(ctx, u.ID, "phc2", ""); !errors.Is(err, ErrPasswordSet) {
		t.Fatalf("err = %v, want ErrPasswordSet", err)
	}
	if got, _ := s.PasswordHash(ctx, u.ID); got != "phc1" {
		t.Fatalf("hash = %q, want phc1", got)
	}
	if err := s.SetFirstPassword(ctx, 999, "x", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateAccountWithIdentity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Now()
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "s@x.co", WorkspaceName: "S", Slug: "s", Verified: true, Now: now,
		Identity: &NewIdentity{Provider: "google", Subject: "g1", Email: "s@gmail.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.IdentityUser(ctx, "google", "g1"); err != nil || got.ID != u.ID {
		t.Fatalf("IdentityUser = %+v, %v", got, err)
	}
	// Collision: nothing of the second account survives.
	before := rawCount(t, s.db, `SELECT COUNT(*) FROM users`)
	wsBefore := rawCount(t, s.db, `SELECT COUNT(*) FROM workspaces`)
	_, _, err = s.CreateAccount(ctx, AccountInput{Email: "t@x.co", WorkspaceName: "T", Slug: "t", Verified: true, Now: now,
		Identity: &NewIdentity{Provider: "google", Subject: "g1", Email: "t@gmail.com"}})
	if !errors.Is(err, ErrIdentityTaken) {
		t.Fatalf("err = %v, want ErrIdentityTaken", err)
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM users`); n != before {
		t.Fatalf("users = %d, want %d", n, before)
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM workspaces`); n != wsBefore {
		t.Fatalf("workspaces = %d, want %d", n, wsBefore)
	}
	// Empty password without an identity is refused.
	if _, _, err := s.CreateAccount(ctx, AccountInput{Email: "n@x.co", WorkspaceName: "N", Slug: "n", Now: now}); err == nil {
		t.Fatal("empty password without identity should fail")
	}
	if n := rawCount(t, s.db, `SELECT COUNT(*) FROM users WHERE email = 'n@x.co'`); n != 0 {
		t.Fatal("user created despite error")
	}
}
