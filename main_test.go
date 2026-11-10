package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/store"

	_ "modernc.org/sqlite"
)

func TestResolveConfig(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "data-config.json")
	system := filepath.Join(dir, "etc-config.json")
	candidates := []string{legacy, system}

	if got := resolveConfig("/explicit.json", candidates); got != "/explicit.json" {
		t.Fatalf("explicit -config must win, got %s", got)
	}
	if got := resolveConfig("", candidates); got != legacy {
		t.Fatalf("with nothing present, report the first candidate, got %s", got)
	}
	os.WriteFile(system, []byte("{}"), 0o600)
	if got := resolveConfig("", candidates); got != system {
		t.Fatalf("new layout only: got %s", got)
	}
	// An existing install with the original /data/config.json keeps working.
	os.WriteFile(legacy, []byte("{}"), 0o600)
	if got := resolveConfig("", candidates); got != legacy {
		t.Fatalf("original layout should be preferred when present, got %s", got)
	}
}

func TestNewSetupLinkAfterSetup(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"db":"`+filepath.Join(dir, "t.db")+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link, err := newSetupLink(cfgPath)
	if err != nil || !strings.Contains(link, "/setup?token=") {
		t.Fatalf("fresh: %q %v", link, err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	tok, _ := st.CreateToken(ctx, store.TokenSetup, 0, time.Hour, now)
	if _, err := st.CompleteSetup(ctx, store.SetupInput{Email: "op@example.com", PasswordHash: "phc", WorkspaceName: "W", SetupToken: tok, Now: now}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := newSetupLink(cfgPath); err == nil || err.Error() != "setup is already complete" {
		t.Fatalf("after setup: %v", err)
	}
}

func TestNewResetLink(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"db":"`+dbPath+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	for _, e := range []string{"a@b.co", "gone@b.co"} {
		if _, _, err := st.CreateAccount(ctx, store.AccountInput{Email: e, PasswordHash: "x", WorkspaceName: e, Slug: e[:1] + e[1:2], Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	if link, err := newResetLink(cfgPath, " A@B.co "); err != nil || !strings.Contains(link, "/reset?token=") {
		t.Fatalf("known user: %q %v", link, err)
	}
	if _, err := newResetLink(cfgPath, "nobody@b.co"); err == nil || err.Error() != "no such user" {
		t.Fatalf("unknown: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE users SET disabled_at = ? WHERE email = 'gone@b.co'`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := newResetLink(cfgPath, "gone@b.co"); err == nil || err.Error() != "user is disabled" {
		t.Fatalf("disabled: %v", err)
	}
}

func TestInviteCLI(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "t.db")
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"db":"`+dbPath+`","base_url":"https://up.example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := newInviteLink(cfgPath, ""); err == nil || err.Error() != "finish setup first" {
		t.Fatalf("before setup: %v", err)
	}
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := st.CreateAccount(ctx, store.AccountInput{Email: "op@b.co", PasswordHash: "x", WorkspaceName: "Op", Slug: "op", Operator: true, Verified: true, Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	link, err := newInviteLink(cfgPath, "")
	if err != nil || !strings.HasPrefix(link, "https://up.example.com/signup?invite=") {
		t.Fatalf("open invite: %q %v", link, err)
	}
	raw := strings.TrimPrefix(link, "https://up.example.com/signup?invite=")
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if tk, err := st.PeekToken(ctx, store.TokenInvite, raw, time.Now()); err != nil || tk.Email != "" {
		t.Fatalf("open invite token: %+v %v", tk, err)
	}
	link, err = newInviteLink(cfgPath, " New@B.co ")
	if err != nil {
		t.Fatal(err)
	}
	raw = link[strings.Index(link, "=")+1:]
	if tk, err := st.PeekToken(ctx, store.TokenInvite, raw, time.Now()); err != nil || tk.Email != "new@b.co" {
		t.Fatalf("bound invite token: %+v %v", tk, err)
	}
	if _, err := newInviteLink(cfgPath, "not-an-email"); err == nil {
		t.Fatal("invalid email should fail")
	}
}
