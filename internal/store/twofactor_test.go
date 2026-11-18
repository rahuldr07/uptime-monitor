package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var tfNow = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// enabled returns a verified user with 2FA turned on, a kept session hash and
// that session's raw token.
func enabled(t *testing.T, s *Store, step int64, codes ...string) (User, string, string) {
	t.Helper()
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	raw, err := s.CreateSession(ctx, u.ID, tfNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTOTP(ctx, u.ID, "SECRETSECRET", tfNow); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTOTP(ctx, u.ID, "SECRETSECRET", step, codes, hashSecret(raw), tfNow); err != nil {
		t.Fatal(err)
	}
	u, err = s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return u, raw, hashSecret(raw)
}

func TestPendingTOTPExpiry(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	if _, err := s.PendingTOTP(ctx, u.ID, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no pending: %v", err)
	}
	if _, err := s.BeginTOTP(ctx, u.ID, "ABC", tfNow); err != nil {
		t.Fatal(err)
	}
	if sec, err := s.PendingTOTP(ctx, u.ID, tfNow.Add(14*time.Minute)); err != nil || sec != "ABC" {
		t.Fatalf("pending = %q, %v", sec, err)
	}
	if _, err := s.PendingTOTP(ctx, u.ID, tfNow.Add(16*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("16 min: %v, want ErrNotFound", err)
	}
	if err := s.EnableTOTP(ctx, u.ID, "ABC", 5, nil, "", tfNow.Add(16*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("enable with expired pending: %v", err)
	}
	// A new Begin replaces the pending secret and restarts the clock.
	if got, err := s.BeginTOTP(ctx, u.ID, "DEF", tfNow.Add(20*time.Minute)); err != nil || got != "DEF" {
		t.Fatalf("begin after expiry = %q, %v", got, err)
	}
	if sec, err := s.PendingTOTP(ctx, u.ID, tfNow.Add(21*time.Minute)); err != nil || sec != "DEF" {
		t.Fatalf("pending = %q, %v", sec, err)
	}
}

func TestEnableTOTPRevokesOthers(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	keep, _ := s.CreateSession(ctx, u.ID, tfNow)
	other, _ := s.CreateSession(ctx, u.ID, tfNow)
	pl, err := s.CreatePendingLogin(ctx, u.ID, "password", "", tfNow, tfNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTOTP(ctx, u.ID, "SEC", tfNow); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTOTP(ctx, u.ID, "SEC", 77, []string{"h1", "h2", "h3"}, hashSecret(keep), tfNow); err != nil {
		t.Fatal(err)
	}
	a, err := s.SessionAuth(ctx, keep, tfNow)
	if err != nil || a.MFAAt == nil || !a.MFAAt.Equal(tfNow) || a.User.TOTPEnabledAt == nil || !a.User.TOTPEnabledAt.Equal(tfNow) {
		t.Fatalf("kept session = %+v, %v", a, err)
	}
	if _, err := s.SessionAuth(ctx, other, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other session survived: %v", err)
	}
	if _, err := s.PendingLogin(ctx, pl, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending login survived: %v", err)
	}
	secret, on, err := s.TOTPSecret(ctx, u.ID)
	if err != nil || !on || secret != "SEC" {
		t.Fatalf("secret = %q %v %v", secret, on, err)
	}
	if _, err := s.PendingTOTP(ctx, u.ID, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("pending secret should be cleared")
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != 3 {
		t.Fatalf("codes left = %d, want 3", n)
	}
	var step int64
	s.db.QueryRow(`SELECT totp_last_step FROM users WHERE id = ?`, u.ID).Scan(&step)
	if step != 77 {
		t.Fatalf("last step = %d, want 77", step)
	}
	// Enabling twice (second tab) fails.
	if err := s.EnableTOTP(ctx, u.ID, "SEC", 78, nil, "", tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second enable: %v", err)
	}
	if _, err := s.BeginTOTP(ctx, u.ID, "X", tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("begin while enabled: %v", err)
	}
}

func TestDisableKeepsLastStep(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, keep, keepHash := enabled(t, s, 500, "h1", "h2")
	other, _ := s.CreateSession(ctx, u.ID, tfNow)
	pl, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", *u.TOTPEnabledAt, tfNow)
	if err := s.DisableTOTP(ctx, u.ID, keepHash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, keep, tfNow); err != nil {
		t.Fatalf("kept session lost: %v", err)
	}
	if _, err := s.SessionAuth(ctx, other, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("other session survived")
	}
	if _, err := s.PendingLogin(ctx, pl, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("pending login survived")
	}
	if sec, on, err := s.TOTPSecret(ctx, u.ID); err != nil || on || sec != "" {
		t.Fatalf("secret = %q %v %v", sec, on, err)
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != 0 {
		t.Fatalf("codes left = %d", n)
	}
	var step int64
	s.db.QueryRow(`SELECT totp_last_step FROM users WHERE id = ?`, u.ID).Scan(&step)
	if step != 500 {
		t.Fatalf("last step = %d, want 500", step)
	}
	// Re-enabling with a lower step never lowers it.
	s.BeginTOTP(ctx, u.ID, "NEW", tfNow)
	if err := s.EnableTOTP(ctx, u.ID, "NEW", 400, nil, keepHash, tfNow); err != nil {
		t.Fatal(err)
	}
	s.db.QueryRow(`SELECT totp_last_step FROM users WHERE id = ?`, u.ID).Scan(&step)
	if step != 500 {
		t.Fatalf("last step lowered to %d", step)
	}
	// Disabling with no kept session signs out everywhere.
	if err := s.DisableTOTP(ctx, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, keep, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("session should be gone")
	}
}

func TestAcceptTOTPStepMonotonic(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	if ok, err := s.AcceptTOTPStep(ctx, newPlain(t, s).ID, "SECRETSECRET", 10); err != nil || ok {
		t.Fatalf("disabled user: %v %v", ok, err)
	}
	u, _, _ := enabled(t, s, 100)
	for _, c := range []struct {
		step int64
		want bool
	}{{100, false}, {99, false}, {101, true}, {101, false}, {105, true}, {104, false}} {
		if ok, err := s.AcceptTOTPStep(ctx, u.ID, "SECRETSECRET", c.step); err != nil || ok != c.want {
			t.Errorf("step %d: %v %v, want %v", c.step, ok, err, c.want)
		}
	}
}

func TestAcceptTOTPStepConcurrent(t *testing.T) {
	s := open(t)
	u, _, _ := enabled(t, s, 100)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.AcceptTOTPStep(context.Background(), u.ID, "SECRETSECRET", 200); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners, want 1", wins.Load())
	}
}

func TestRecoveryCodeOnce(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, keepHash := enabled(t, s, 1, "aaa", "bbb", "ccc")
	if ok, err := s.UseRecoveryCode(ctx, u.ID, "aaa", tfNow); err != nil || !ok {
		t.Fatalf("first use: %v %v", ok, err)
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "aaa", tfNow); ok {
		t.Fatal("second use accepted")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "zzz", tfNow); ok {
		t.Fatal("unknown code accepted")
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != 2 {
		t.Fatalf("left = %d, want 2", n)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := s.UseRecoveryCode(ctx, u.ID, "bbb", tfNow); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d winners, want 1", wins.Load())
	}
	// Regenerating replaces all codes, used or not, and signs out others.
	other, _ := s.CreateSession(ctx, u.ID, tfNow)
	if err := s.ReplaceRecoveryCodes(ctx, u.ID, []string{"ddd", "eee"}, keepHash); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.RecoveryCodesLeft(ctx, u.ID); n != 2 {
		t.Fatalf("left = %d, want 2", n)
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, "ccc", tfNow); ok {
		t.Fatal("old code survived replacement")
	}
	if _, err := s.SessionAuth(ctx, other, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("other session survived")
	}
	// A user without 2FA cannot be given codes.
	plain := newPlain(t, s)
	if err := s.ReplaceRecoveryCodes(ctx, plain.ID, []string{"x"}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace without 2FA: %v", err)
	}
}

func newPlain(t *testing.T, s *Store) User {
	t.Helper()
	u, _, err := s.CreateAccount(context.Background(), AccountInput{Email: "p@b.co", PasswordHash: "x", WorkspaceName: "P", Slug: "p", Now: tfNow})
	if err != nil {
		t.Fatal(err)
	}
	if err := markVerified(s, u.ID, tfNow); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPendingLoginTries(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, _ := enabled(t, s, 1)
	on := *u.TOTPEnabledAt
	raw, err := s.CreatePendingLogin(ctx, u.ID, "google", "/settings", on, tfNow)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PendingLogin(ctx, raw, tfNow)
	if err != nil || p.UserID != u.ID || p.Method != "google" || p.Next != "/settings" || p.Tries != 0 || !p.TOTPEnabledAt.Equal(on) {
		t.Fatalf("pending = %+v, %v", p, err)
	}
	for i := 1; i <= 5; i++ {
		p, err := s.TryPendingLogin(ctx, raw, tfNow)
		if err != nil || p.Tries != i {
			t.Fatalf("try %d: %+v, %v", i, p, err)
		}
	}
	if _, err := s.TryPendingLogin(ctx, raw, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("6th try: %v, want ErrNotFound", err)
	}

	// Concurrent tries never exceed five.
	raw2, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	var granted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.TryPendingLogin(ctx, raw2, tfNow); err == nil {
				granted.Add(1)
			}
		}()
	}
	wg.Wait()
	if granted.Load() != 5 {
		t.Fatalf("%d tries granted, want 5", granted.Load())
	}

	// Expired.
	raw3, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	if _, err := s.TryPendingLogin(ctx, raw3, tfNow.Add(5*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired try: %v", err)
	}
	if _, err := s.PendingLogin(ctx, raw3, tfNow.Add(5*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired peek: %v", err)
	}
	// Used: Finish is single-use, then nothing works.
	raw4, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	if err := s.FinishPendingLogin(ctx, raw4, tfNow); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishPendingLogin(ctx, raw4, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second finish: %v", err)
	}
	if _, err := s.TryPendingLogin(ctx, raw4, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("used try: %v", err)
	}
	if _, err := s.PendingLogin(ctx, raw4, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("used peek: %v", err)
	}
	// Unknown token, and a row whose 2FA stamp no longer matches.
	if _, err := s.PendingLogin(ctx, "nope", tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	raw5, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on.Add(time.Hour), tfNow)
	if _, err := s.TryPendingLogin(ctx, raw5, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale 2FA stamp: %v", err)
	}
	// Delete.
	raw6, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	if err := s.DeletePendingLogin(ctx, raw6); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingLogin(ctx, raw6, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}

func TestPrunePendingLogins(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, _ := enabled(t, s, 1)
	on := *u.TOTPEnabledAt
	old, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	fresh, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow.Add(10*time.Minute))
	n, err := s.PrunePendingLogins(ctx, tfNow.Add(11*time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want 1", n, err)
	}
	if _, err := s.PendingLogin(ctx, fresh, tfNow.Add(11*time.Minute)); err != nil {
		t.Fatalf("fresh row pruned: %v", err)
	}
	_ = old
}

func TestPendingLoginCascade(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, _ := enabled(t, s, 1, "aaa")
	if _, err := s.CreatePendingLogin(ctx, u.ID, "password", "", *u.TOTPEnabledAt, tfNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA foreign_keys`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"pending_logins", "recovery_codes"} {
		var n int
		s.db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n)
		if n != 0 {
			t.Fatalf("%s rows after user delete = %d", tbl, n)
		}
	}
}

func TestMarkMFAAndCreateSessionMFA(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	raw, _ := s.CreateSession(ctx, u.ID, tfNow)
	a, _ := s.SessionAuth(ctx, raw, tfNow)
	if a.MFAAt != nil {
		t.Fatalf("plain session has mfa_at %v", a.MFAAt)
	}
	later := tfNow.Add(time.Minute)
	if err := s.MarkMFA(ctx, a.SessionHash, later); err != nil {
		t.Fatal(err)
	}
	a, _ = s.SessionAuth(ctx, raw, later)
	if a.MFAAt == nil || !a.MFAAt.Equal(later) {
		t.Fatalf("mfa_at = %v", a.MFAAt)
	}
	if err := s.MarkMFA(ctx, "nope", later); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	raw2, err := s.CreateSessionMFA(ctx, u.ID, later)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := s.SessionAuth(ctx, raw2, later)
	if err != nil || a2.MFAAt == nil || !a2.MFAAt.Equal(later) || a2.AuthAt == nil || !a2.AuthAt.Equal(later) {
		t.Fatalf("session = %+v, %v", a2, err)
	}
}

func TestBeginTOTPReusesValidPending(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	a, err := s.BeginTOTP(ctx, u.ID, "FIRST", tfNow)
	if err != nil || a != "FIRST" {
		t.Fatalf("first = %q, %v", a, err)
	}
	b, err := s.BeginTOTP(ctx, u.ID, "SECOND", tfNow.Add(14*time.Minute))
	if err != nil || b != "FIRST" {
		t.Fatalf("within 15 min = %q, %v; want the same secret", b, err)
	}
	// Reusing does not extend the lifetime.
	if _, err := s.PendingTOTP(ctx, u.ID, tfNow.Add(16*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reuse extended the pending secret: %v", err)
	}
	c, err := s.BeginTOTP(ctx, u.ID, "THIRD", tfNow.Add(16*time.Minute))
	if err != nil || c != "THIRD" {
		t.Fatalf("after expiry = %q, %v", c, err)
	}
}

func TestEnableTOTPNeedsTheConfirmedSecret(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	s.BeginTOTP(ctx, u.ID, "MINE", tfNow)
	for _, sec := range []string{"OTHER", ""} {
		if err := s.EnableTOTP(ctx, u.ID, sec, 5, nil, "", tfNow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("enable with %q: %v", sec, err)
		}
	}
	if _, on, _ := s.TOTPSecret(ctx, u.ID); on {
		t.Fatal("2FA turned on with the wrong secret")
	}
	// The pending secret was replaced after the user confirmed an older one.
	s.BeginTOTP(ctx, u.ID, "NEWER", tfNow.Add(20*time.Minute))
	if err := s.EnableTOTP(ctx, u.ID, "MINE", 5, nil, "", tfNow.Add(20*time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale secret: %v", err)
	}
	if _, on, _ := s.TOTPSecret(ctx, u.ID); on {
		t.Fatal("2FA turned on with a stale secret")
	}
}

func TestAcceptTOTPStepBindsSecret(t *testing.T) {
	s := open(t)
	u, _, _ := enabled(t, s, 100)
	if ok, err := s.AcceptTOTPStep(context.Background(), u.ID, "NOTTHESECRET", 200); err != nil || ok {
		t.Fatalf("wrong secret accepted: %v %v", ok, err)
	}
	if ok, _ := s.AcceptTOTPStep(context.Background(), u.ID, "SECRETSECRET", 200); !ok {
		t.Fatal("right secret refused")
	}
}

func TestDisableTOTPNeedsSomethingToDisable(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u := newVerified(t, s, tfNow)
	raw, _ := s.CreateSession(ctx, u.ID, tfNow)
	if err := s.DisableTOTP(ctx, u.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable with 2FA off: %v", err)
	}
	if _, err := s.SessionAuth(ctx, raw, tfNow); err != nil {
		t.Fatalf("sessions revoked by a no-op disable: %v", err)
	}
	// A pending secret alone can be cleared.
	s.BeginTOTP(ctx, u.ID, "PEND", tfNow)
	if err := s.DisableTOTP(ctx, u.ID, hashSecret(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingTOTP(ctx, u.ID, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("pending secret survived")
	}
}

func TestKeepSessionOfAnotherUserFailsSafe(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, mine, _ := enabled(t, s, 1)
	other := newPlain(t, s)
	theirs, _ := s.CreateSession(ctx, other.ID, tfNow)
	if err := s.DisableTOTP(ctx, u.ID, hashSecret(theirs)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, mine, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatal("user's sessions should all be gone")
	}
	if _, err := s.SessionAuth(ctx, theirs, tfNow); err != nil {
		t.Fatalf("other user's session touched: %v", err)
	}
}

func TestTryPendingLoginRowGone(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, _ := enabled(t, s, 1)
	raw, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", *u.TOTPEnabledAt, tfNow)
	if err := s.DeletePendingLogin(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TryPendingLogin(ctx, raw, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted row: %v, want ErrNotFound", err)
	}
	if _, err := s.TryPendingLogin(ctx, raw, tfNow); errors.Is(err, sql.ErrNoRows) {
		t.Fatal("raw sql.ErrNoRows leaked")
	}
}

func TestPendingLoginNeedsLiveUser(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, _ := enabled(t, s, 1)
	raw, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", *u.TOTPEnabledAt, tfNow)
	if _, err := s.SetUserDisabled(ctx, u.ID, true, tfNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingLogin(ctx, raw, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled user's row: %v", err)
	}
	// Enabling again does not resurrect it: the rows were deleted.
	s.SetUserDisabled(ctx, u.ID, false, tfNow)
	if _, err := s.PendingLogin(ctx, raw, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("row survived disable/enable: %v", err)
	}
	// Unverified user.
	raw2, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", *u.TOTPEnabledAt, tfNow)
	s.db.Exec(`UPDATE users SET verified_at = NULL WHERE id = ?`, u.ID)
	if _, err := s.PendingLogin(ctx, raw2, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unverified user's row: %v", err)
	}
}

func TestPasswordChangeDeletesPendingLogins(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, keep := enabled(t, s, 1)
	on := *u.TOTPEnabledAt
	raw, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	if err := s.SetPassword(ctx, u.ID, "newhash", keep); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingLogin(ctx, raw, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetPassword left the row: %v", err)
	}
	raw, _ = s.CreatePendingLogin(ctx, u.ID, "password", "", on, tfNow)
	tok, err := s.CreateToken(ctx, TokenReset, u.ID, time.Hour, tfNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, tok, "newer", tfNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingLogin(ctx, raw, tfNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ResetPassword left the row: %v", err)
	}
}

func TestSessionRevokeKeepsPendingLogins(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	u, _, _ := enabled(t, s, 1)
	raw, _ := s.CreatePendingLogin(ctx, u.ID, "password", "", *u.TOTPEnabledAt, tfNow)
	if err := s.DeleteUserSessions(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingLogin(ctx, raw, tfNow); err != nil {
		t.Fatalf("session revoke removed a pending login: %v", err)
	}
}

// A sign-in that created its plain session just before 2FA was enabled must
// not authenticate afterwards; the session that proved the code does.
func TestSessionAuthNeedsMFAWhenTwoFactorOn(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	now := time.Now()
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "aa", Verified: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	keptAuth, err := s.SessionAuth(ctx, kept, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginTOTP(ctx, u.ID, "JBSWY3DPEHPK3PXP", now); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableTOTP(ctx, u.ID, "JBSWY3DPEHPK3PXP", 1, []string{"h"}, keptAuth.SessionHash, now); err != nil {
		t.Fatal(err)
	}
	// Simulate the race: a plain session created after the revoke.
	late, err := s.CreateSession(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, late, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("plain session of a 2FA account authenticates: %v", err)
	}
	if _, err := s.SessionAuth(ctx, plain, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked session authenticates: %v", err)
	}
	if _, err := s.SessionAuth(ctx, kept, now); err != nil {
		t.Errorf("the session that enabled 2FA lost access: %v", err)
	}
	mfa, err := s.CreateSessionMFA(ctx, u.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionAuth(ctx, mfa, now); err != nil {
		t.Errorf("a code-verified session is refused: %v", err)
	}
	// Turned off: plain sessions work again.
	if err := s.DisableTOTP(ctx, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	p2, _ := s.CreateSession(ctx, u.ID, now)
	if _, err := s.SessionAuth(ctx, p2, now); err != nil {
		t.Errorf("plain session after turning off: %v", err)
	}
}

func TestClearPendingTOTP(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	now := time.Now()
	u, _, err := s.CreateAccount(ctx, AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "aa", Verified: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := s.CreateSession(ctx, u.ID, now)
	if _, err := s.BeginTOTP(ctx, u.ID, "JBSWY3DPEHPK3PXP", now); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearPendingTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PendingTOTP(ctx, u.ID, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("pending survived: %v", err)
	}
	if _, err := s.SessionAuth(ctx, raw, now); err != nil {
		t.Errorf("a session was touched: %v", err)
	}
	if err := s.ClearPendingTOTP(ctx, u.ID); err != nil {
		t.Errorf("clearing nothing: %v", err)
	}
}
