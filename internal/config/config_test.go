package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaultsAndEnv(t *testing.T) {
	t.Setenv("TEST_TOKEN", "abc123")
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	os.WriteFile(p, []byte(`{"targets":[{"name":"a","url":"https://a.test"}],
		"notify":{"telegram":{"bot_token":"${TEST_TOKEN}","chat_id":"1"}}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.DB != "uptime.db" || c.FailureThreshold != 2 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if c.Interval.Duration != 30*time.Second || c.Targets[0].Interval.Duration != 30*time.Second {
		t.Errorf("interval default wrong: %v / %v", c.Interval, c.Targets[0].Interval)
	}
	if c.Notify.Telegram.BotToken != "abc123" {
		t.Errorf("env not expanded: %q", c.Notify.Telegram.BotToken)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		`{"targets":[{"name":"","url":"https://a"}]}`:                                 "has no name",
		`{"targets":[{"name":"a","url":"ftp://a"}]}`:                                  "invalid url",
		`{"targets":[{"name":"a","url":"https://a"},{"name":"a","url":"https://b"}]}`: "duplicate",
		`{"targets":[{"name":"a","url":"https://a","interval":"5s","timeout":"6s"}]}`: "exceeds interval",
		`{"targets":[{"name":"a","url":"https://a","expect_status":99}]}`:             "expect_status",
		`{"targets":[],"notify":{"slack":{"webhook_url":"http://x"}}}`:                "https webhook",
		`{"targets":[],"notify":{"email":{"smtp_host":"h"}}}`:                         "email notifier",
		`{"interval":5}`: "duration must be a string",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want containing %q", in, err, want)
		}
	}
}

func TestNoTargetsIsAllowed(t *testing.T) {
	if _, err := Parse([]byte(`{}`)); err != nil {
		t.Fatalf("empty config should be valid (targets can be added in the UI): %v", err)
	}
}
