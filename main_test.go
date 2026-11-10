package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/store"
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
