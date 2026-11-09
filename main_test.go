package main

import (
	"os"
	"path/filepath"
	"testing"
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
