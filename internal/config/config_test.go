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
		{"path", `{"base_url":"https://x/y"}`, "base_url must be"},
		{"scheme", `{"base_url":"ftp://x"}`, "base_url must be"},
		{"relative", `{"base_url":"status.example.com"}`, "base_url must be"},
		{"query", `{"base_url":"https://x?a=1"}`, "base_url must be"},
		{"fragment", `{"base_url":"https://x#f"}`, "base_url must be"},
		{"user", `{"base_url":"https://user@host"}`, "base_url must be"},
		{"userpass", `{"base_url":"https://u:p@host"}`, "base_url must be"},
		{"spoof", `{"base_url":"https://status.example.com@evil.example"}`, "base_url must be"},
		{"noHost", `{"base_url":"https://:443"}`, "base_url must be"},
		{"emptyPort", `{"base_url":"https://x:"}`, "base_url must be"},
		{"bigPort", `{"base_url":"https://x:99999"}`, "base_url must be"},
		{"emptyFrag", `{"base_url":"https://x#"}`, "base_url must be"},
		{"emptyQuery", `{"base_url":"https://x?"}`, "base_url must be"},
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
	c, err = Parse([]byte(`{"base_url":"HTTPS://Status.Example.com/"}`))
	if err != nil || c.BaseURL != "https://Status.Example.com" || !c.HTTPSBase() {
		t.Errorf("mixed-case base_url: %v %q", err, c.BaseURL)
	}
	for listen, want := range map[string]string{
		":8080":                "http://localhost:8080",
		"127.0.0.1:9":          "http://127.0.0.1:9",
		"0.0.0.0:80":           "http://localhost:80",
		"[::]:81":              "http://localhost:81",
		"[0:0:0:0:0:0:0:0]:80": "http://localhost:80",
		"[fe80::1%eth0]:80":    "http://[fe80::1%25eth0]:80",
		"[::1]:82":             "http://[::1]:82",
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

func TestSignupModes(t *testing.T) {
	if _, err := Parse([]byte(`{"signup":"invite","base_url":"https://status.example.com"}`)); err != nil {
		t.Errorf("invite: %v", err)
	}
	for _, mode := range []string{"open", "bogus"} {
		_, err := Parse([]byte(`{"signup":"` + mode + `","base_url":"https://status.example.com"}`))
		if err == nil || !strings.Contains(err.Error(), `signup "`+mode+`" is not supported yet`) {
			t.Errorf("%s: err = %v, want not supported yet", mode, err)
		}
	}
}

func TestLimitsDefaults(t *testing.T) {
	c, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	l := c.Limits
	if l.MaxMonitors == nil || *l.MaxMonitors != 20 || l.MaxWorkspaces == nil || *l.MaxWorkspaces != 0 ||
		l.SignupsPerHour == nil || *l.SignupsPerHour != 100 || l.HostRequestsPerSecond == nil || *l.HostRequestsPerSecond != 1 ||
		l.MailPerHour == nil || *l.MailPerHour != 500 || l.MinInterval.Duration != 30*time.Second {
		t.Errorf("defaults = %+v", l)
	}
	c, err = Parse([]byte(`{"limits":{"max_monitors":0,"min_interval":"1m","max_workspaces":5,"signups_per_hour":7,"host_requests_per_second":0.5}}`))
	if err != nil {
		t.Fatal(err)
	}
	l = c.Limits
	if *l.MaxMonitors != 0 || l.MinInterval.Duration != time.Minute || *l.MaxWorkspaces != 5 || *l.SignupsPerHour != 7 || *l.HostRequestsPerSecond != 0.5 {
		t.Errorf("explicit = %+v", l)
	}
}

func TestLimitsNullGetsDefault(t *testing.T) {
	c, err := Parse([]byte(`{"limits":{"max_monitors":null,"min_interval":null,"host_requests_per_second":null,"signups_per_hour":null}}`))
	if err != nil {
		t.Fatal(err)
	}
	l := c.Limits
	if *l.MaxMonitors != 20 || l.MinInterval.Duration != 30*time.Second || *l.HostRequestsPerSecond != 1 || *l.SignupsPerHour != 100 {
		t.Errorf("null limits = %+v", l)
	}
}

func TestLimitsValidation(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"max_monitors", `{"limits":{"max_monitors":-1}}`, "limits.max_monitors"},
		{"max_workspaces", `{"limits":{"max_workspaces":-1}}`, "limits.max_workspaces"},
		{"signups_per_hour", `{"limits":{"signups_per_hour":-1}}`, "limits.signups_per_hour must be at least 1"},
		{"signups_per_hour zero", `{"limits":{"signups_per_hour":0}}`, "limits.signups_per_hour must be at least 1"},
		{"host_requests_per_second", `{"limits":{"host_requests_per_second":-1}}`, "limits.host_requests_per_second must be positive"},
		{"host_requests_per_second zero", `{"limits":{"host_requests_per_second":0}}`, "limits.host_requests_per_second must be positive"},
		{"min_interval negative", `{"limits":{"min_interval":"-1m"}}`, "limits.min_interval"},
		{"min_interval", `{"limits":{"min_interval":"500ms"}}`, "limits.min_interval"},
		{"mail_per_hour", `{"limits":{"mail_per_hour":10}}`, "limits.mail_per_hour is not supported yet"},
		{"mail_per_hour zero", `{"limits":{"mail_per_hour":0}}`, "limits.mail_per_hour is not supported yet"},
	} {
		if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), "config: ") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}

func TestSignupNeedsBaseURL(t *testing.T) {
	for _, tc := range []struct {
		base string
		ok   bool
	}{
		{"", false},
		{"http://status.example.com", false},
		{"http://localhost:8080", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://localhost.:8080", true},
		{"http://127.0.0.2:8080", true},
		{"https://status.example.com", true},
	} {
		in := `{"signup":"invite"`
		if tc.base != "" {
			in += `,"base_url":"` + tc.base + `"`
		}
		_, err := Parse([]byte(in + `}`))
		if tc.ok && err != nil {
			t.Errorf("%q: %v", tc.base, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "config: ")) {
			t.Errorf("%q: err = %v, want config error", tc.base, err)
		}
	}
}

func TestValidateNotifyRestricted(t *testing.T) {
	goodTok := "123456:" + strings.Repeat("A", 30)
	mail := func(mod func(e *Email)) *Notify {
		e := &Email{SMTPHost: "h", SMTPPort: 587, Username: "u", From: "a@b", To: []string{"c@d"}}
		mod(e)
		return &Notify{Email: e}
	}
	cases := []struct {
		name string
		n    *Notify
		want string // "" = valid
	}{
		{"ok telegram", &Notify{Telegram: &Telegram{BotToken: goodTok, ChatID: "1"}}, ""},
		{"short token", &Notify{Telegram: &Telegram{BotToken: "123:abc", ChatID: "1"}}, "telegram bot_token"},
		{"token without id", &Notify{Telegram: &Telegram{BotToken: strings.Repeat("A", 40), ChatID: "1"}}, "telegram bot_token"},
		{"ok email", mail(func(e *Email) {}), ""},
		{"port 465", mail(func(e *Email) { e.SMTPPort = 465 }), ""},
		{"port 25", mail(func(e *Email) { e.SMTPPort = 25 }), "smtp_port"},
		{"default port ok", mail(func(e *Email) { e.SMTPPort = 0 }), ""},
		{"no username", mail(func(e *Email) { e.Username = "" }), "username"},
		{"6 recipients", mail(func(e *Email) { e.To = []string{"1@x", "2@x", "3@x", "4@x", "5@x", "6@x"} }), "at most 5"},
		{"5 recipients", mail(func(e *Email) { e.To = []string{"1@x", "2@x", "3@x", "4@x", "5@x"} }), ""},
		{"base rules still apply", &Notify{Slack: &Slack{WebhookURL: "http://x"}}, "https"},
	}
	for _, c := range cases {
		err := ValidateNotifyRestricted(c.n)
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: err=%v, want %q", c.name, err, c.want)
		}
	}
}
