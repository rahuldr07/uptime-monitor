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
		`{"interval":5}`:      "duration must be a string",
		`{"retention":"12h"}`: "at least 24h",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want containing %q", in, err, want)
		}
	}
}

func TestCheckAuth(t *testing.T) {
	cases := []struct {
		listen, password string
		insecure, ok     bool
	}{
		{":8080", "", false, false},
		{"0.0.0.0:8080", "", false, false},
		{"192.168.1.5:8080", "", false, false},
		{":8080", "", true, true},
		{":8080", "pw", false, true},
		{"127.0.0.1:8080", "", false, true},
		{"[::1]:8080", "", false, true},
		{"localhost:8080", "", false, true},
	}
	for _, c := range cases {
		cfg := &Config{Listen: c.listen, AdminPassword: c.password, InsecureNoAuth: c.insecure}
		if err := cfg.CheckAuth(); (err == nil) != c.ok {
			t.Errorf("listen=%q password=%q insecure=%v: got %v, want ok=%v", c.listen, c.password, c.insecure, err, c.ok)
		}
	}
}

func TestTargetNameControlChars(t *testing.T) {
	if err := ValidateTarget("a\r\nBcc: x@y", "https://a", time.Minute, time.Second, 0); err == nil {
		t.Fatal("control characters in a name should be rejected")
	}
}

func TestUnsetChannelVariablesMeanNoChannel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"notify":{"telegram":{"bot_token":"${UNSET_TG_TOKEN_X}","chat_id":"${UNSET_TG_CHAT_X}"},
		"slack":{"webhook_url":"${UNSET_SLACK_X}"},"email":{"smtp_host":"","to":[]}}}`), 0o600)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("blank channels from unset variables should not fail startup: %v", err)
	}
	if c.Notify.Telegram != nil || c.Notify.Slack != nil || c.Notify.Email != nil {
		t.Fatalf("blank channels should be dropped: %+v", c.Notify)
	}
	os.WriteFile(p, []byte(`{"notify":{"email":{"to":["${UNSET_OPS_EMAIL_X}"]}}}`), 0o600)
	if c, err := Load(p); err != nil || c.Notify.Email != nil {
		t.Fatalf("an email channel whose only recipient is unset should be dropped: %v %+v", err, c)
	}
	// A half-filled channel is still a mistake worth reporting.
	if _, err := Parse([]byte(`{"notify":{"telegram":{"bot_token":"t"}}}`)); err == nil {
		t.Fatal("telegram without chat_id should be rejected")
	}
}

func TestNoTargetsIsAllowed(t *testing.T) {
	if _, err := Parse([]byte(`{}`)); err != nil {
		t.Fatalf("empty config should be valid (targets can be added in the UI): %v", err)
	}
}

func TestAllowPrivateTargets(t *testing.T) {
	c, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowPrivateTargets != "operator" {
		t.Fatalf("default = %q, want operator", c.AllowPrivateTargets)
	}
	if !c.AllowPrivate(true) || c.AllowPrivate(false) {
		t.Fatal("operator mode should allow only the operator")
	}
	c, err = Parse([]byte(`{"allow_private_targets":"none"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowPrivate(true) {
		t.Fatal("none should block the operator too")
	}
	c, err = Parse([]byte(`{"allow_private_targets":"all"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AllowPrivate(false) {
		t.Fatal("all should allow everyone")
	}
	if _, err = Parse([]byte(`{"allow_private_targets":"some"}`)); err == nil || !strings.Contains(err.Error(), "allow_private_targets must be") {
		t.Fatalf("err = %v, want allow_private_targets must be", err)
	}
}

func TestAccountsConfigKeys(t *testing.T) {
	c, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Signup != "closed" {
		t.Errorf("default signup = %q, want closed", c.Signup)
	}
	for _, tc := range []struct{ name, in, want string }{
		{"signup open", `{"signup":"open"}`, "not supported yet"},
		{"mail", `{"mail":{}}`, "mail is not supported yet"},
		{"limits", `{"limits":{}}`, "limits is not supported yet"},
		{"path", `{"base_url":"https://x/y"}`, "base_url must be"},
		{"scheme", `{"base_url":"ftp://x"}`, "base_url must be"},
		{"relative", `{"base_url":"status.example.com"}`, "base_url must be"},
		{"query", `{"base_url":"https://x?a=1"}`, "base_url must be"},
		{"fragment", `{"base_url":"https://x#f"}`, "base_url must be"},
	} {
		if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
	c, err = Parse([]byte(`{"base_url":"https://status.example.com/"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://status.example.com" || !c.HTTPSBase() || c.LinkBase() != "https://status.example.com" {
		t.Errorf("base_url = %q https=%v link=%q", c.BaseURL, c.HTTPSBase(), c.LinkBase())
	}
	for listen, want := range map[string]string{
		":8080":       "http://localhost:8080",
		"127.0.0.1:9": "http://127.0.0.1:9",
		"0.0.0.0:80":  "http://localhost:80",
		"[::]:81":     "http://localhost:81",
		"[::1]:82":    "http://[::1]:82",
	} {
		c, err := Parse([]byte(`{"listen":"` + listen + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.LinkBase(); got != want || c.HTTPSBase() {
			t.Errorf("LinkBase(%q) = %q, want %q", listen, got, want)
		}
	}
}
