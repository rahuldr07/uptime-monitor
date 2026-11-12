package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/config"
	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/web"

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

func TestInviteEmailArg(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
		bad  bool
	}{
		{nil, "", false},
		{[]string{"a@b.co"}, "a@b.co", false},
		{[]string{"a@b.co", "extra"}, "", true},
		{[]string{"-config"}, "", true},
		{[]string{"-config", "x.json"}, "", true},
	} {
		got, err := inviteEmailArg(c.args)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("%v: got %q, %v", c.args, got, err)
		}
	}
}

func TestUserAgentString(t *testing.T) {
	if got := userAgent("dev", "https://status.example.com"); got != "uptime-monitor/dev (+https://status.example.com)" {
		t.Errorf("with base_url: %q", got)
	}
	if got := userAgent("1.2.3", ""); got != "uptime-monitor/1.2.3" {
		t.Errorf("without base_url: %q", got)
	}
}

func TestNewMailer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	perHour := 500
	cfg := &config.Config{Limits: config.Limits{MailPerHour: &perHour}}
	var m web.Mailer = newMailer(ctx, cfg, slog.Default())
	if m != nil {
		t.Fatalf("without mail, newMailer must return a nil interface, got %#v", m)
	}
	tls := true
	cfg.Mail = &config.Mail{SMTPHost: "smtp.example.com", SMTPPort: 587, From: "a@example.com", RequireTLS: &tls}
	if m = newMailer(ctx, cfg, slog.Default()); m == nil {
		t.Fatal("with mail, newMailer must return a mailer")
	}
}

func TestEnableUser(t *testing.T) {
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
	_, _, err = st.CreateAccount(ctx, store.AccountInput{Email: "a@b.co", PasswordHash: "x", WorkspaceName: "A", Slug: "aa", Verified: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.CreateAccount(ctx, store.AccountInput{Email: "op@b.co", PasswordHash: "x", WorkspaceName: "Op", Slug: "op", Operator: true, Verified: true, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	u, _, err := st.UserByEmail(ctx, "a@b.co")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.SetUserDisabled(ctx, u.ID, true, now)
	if err != nil || len(ids) != 1 {
		t.Fatalf("disable: suspended %v, %v", ids, err)
	}
	st.Close()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// A disabled operator cannot be disabled through the store, so set it directly.
	if _, err := db.Exec(`UPDATE users SET disabled_at = ? WHERE email = 'op@b.co'`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := enableUser(cfgPath, "a@b.co"); err != nil {
		t.Fatalf("enable user: %v", err)
	}
	if err := enableUser(cfgPath, "op@b.co"); err != nil {
		t.Fatalf("enable operator: %v", err)
	}
	if err := enableUser(cfgPath, "nobody@b.co"); err == nil || err.Error() != "no such user" {
		t.Fatalf("unknown: %v", err)
	}

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, e := range []string{"a@b.co", "op@b.co"} {
		u, _, err := st.UserByEmail(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if u.DisabledAt != nil {
			t.Errorf("%s still disabled", e)
		}
	}
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM workspaces WHERE suspended_at IS NOT NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d workspaces still suspended", n)
	}
}
