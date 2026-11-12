package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSessionAuthAtAndHasPassword(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	u := newVerified(t, s, now)
	raw, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.SessionAuth(ctx, raw, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if a.AuthAt == nil || !a.AuthAt.Equal(now) || !a.HasPassword {
		t.Fatalf("auth = AuthAt %v HasPassword %v", a.AuthAt, a.HasPassword)
	}

	// A session that predates v3 has NULL auth_at.
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET auth_at = NULL`); err != nil {
		t.Fatal(err)
	}
	a, err = s.SessionAuth(ctx, raw, now.Add(time.Minute))
	if err != nil || a.AuthAt != nil {
		t.Fatalf("AuthAt = %v, %v; want nil", a.AuthAt, err)
	}

	// MarkReauth sets it again.
	later := now.Add(2 * time.Minute)
	if err := s.MarkReauth(ctx, a.SessionHash, later); err != nil {
		t.Fatal(err)
	}
	a, err = s.SessionAuth(ctx, raw, later)
	if err != nil || a.AuthAt == nil || !a.AuthAt.Equal(later) {
		t.Fatalf("AuthAt = %v, %v; want %v", a.AuthAt, err, later)
	}
	if err := s.MarkReauth(ctx, "nope", later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	// A social-only account reports HasPassword false.
	so, _, err := s.CreateAccount(ctx, AccountInput{Email: "so@x.co", WorkspaceName: "So", Slug: "so", Verified: true, Now: now,
		Identity: &NewIdentity{Provider: "google", Subject: "g", Email: "so@x.co"}})
	if err != nil {
		t.Fatal(err)
	}
	raw2, _ := s.CreateSession(ctx, so.ID, now)
	a, err = s.SessionAuth(ctx, raw2, now)
	if err != nil || a.HasPassword {
		t.Fatalf("HasPassword = %v, %v; want false", a.HasPassword, err)
	}
}
