// Package config loads and validates the monitor configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Duration is a time.Duration that unmarshals from strings like "30s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil // leave unset, so the default applies
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

// Target is one URL to watch.
type Target struct {
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	Interval     Duration `json:"interval"`
	Timeout      Duration `json:"timeout"`
	ExpectStatus int      `json:"expect_status"` // 0 means any 2xx or 3xx
	Keyword      string   `json:"keyword"`       // optional text that must appear in the body
	Group        string   `json:"group"`         // optional grouping on the status page
}

// Telegram bot settings.
type Telegram struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Slack incoming-webhook settings.
type Slack struct {
	WebhookURL string `json:"webhook_url"`
	Disabled   bool   `json:"disabled,omitempty"`
}

// Email SMTP settings.
type Email struct {
	SMTPHost string   `json:"smtp_host"`
	SMTPPort int      `json:"smtp_port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Disabled bool     `json:"disabled,omitempty"`
}

// Notify groups the optional alert channels.
type Notify struct {
	Telegram *Telegram `json:"telegram,omitempty"`
	Slack    *Slack    `json:"slack,omitempty"`
	Email    *Email    `json:"email,omitempty"`
}

// Config is the full application configuration.
type Config struct {
	Listen              string   `json:"listen"`
	DB                  string   `json:"db"`
	AdminPassword       string   `json:"admin_password"`        // unlocks setup on an upgraded install; ignored once an account exists
	InsecureNoAuth      bool     `json:"insecure_no_auth"`      // no effect: sign-in is always required (main logs a warning)
	TrustProxy          bool     `json:"trust_proxy"`           // trust X-Forwarded-For/-Proto from a reverse proxy
	TrustedProxies      int      `json:"trusted_proxies"`       // proxies in front of the server (default 1 with trust_proxy)
	AllowPrivateTargets string   `json:"allow_private_targets"` // none, operator or all: who may reach internal addresses (default operator)
	FailureThreshold    int      `json:"failure_threshold"`     // consecutive failures before "down"
	Interval            Duration `json:"interval"`
	Timeout             Duration `json:"timeout"`
	Retention           Duration `json:"retention"`
	Targets             []Target `json:"targets"`
	Notify              Notify   `json:"notify"`

	BaseURL string          `json:"base_url"` // public origin for links; empty derives one from listen
	Signup  string          `json:"signup"`   // closed (default) or invite; open arrives with mail
	Mail    json.RawMessage `json:"mail"`     // rejected until supported
	Limits  Limits          `json:"limits"`
}

// Limits caps what accounts may do. Pointer fields tell "not given" (default)
// from an explicit 0; applyDefaults makes every one non-nil.
type Limits struct {
	MaxMonitors           *int     `json:"max_monitors"`             // per workspace; 0 is unlimited (default 20)
	MinInterval           Duration `json:"min_interval"`             // per workspace (default 30s)
	MaxWorkspaces         *int     `json:"max_workspaces"`           // 0 is unlimited (default)
	SignupsPerHour        *int     `json:"signups_per_hour"`         // across the instance (default 100)
	HostRequestsPerSecond *float64 `json:"host_requests_per_second"` // per target host (default 1)
	MailPerHour           *int     `json:"mail_per_hour"`            // rejected until mail is supported
}

// Load reads a JSON config file. Environment variables in the form $VAR or
// ${VAR} are expanded before parsing, so secrets can stay out of the file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse([]byte(os.ExpandEnv(string(raw))))
}

// Parse decodes config JSON, applies defaults and validates it.
func Parse(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ProxyHops is how many reverse proxies' X-Forwarded-For entries to trust,
// or 0 when trust_proxy is off.
func (c *Config) ProxyHops() int {
	if !c.TrustProxy {
		return 0
	}
	if c.TrustedProxies > 0 {
		return c.TrustedProxies
	}
	return 1
}

// AllowPrivate reports whether a client may reach loopback, private and other
// internal addresses. operator says whether the client belongs to the operator.
func (c *Config) AllowPrivate(operator bool) bool {
	switch c.AllowPrivateTargets {
	case "none":
		return false
	case "all":
		return true
	}
	return operator
}

// HTTPSBase reports whether links are built on an https base URL.
func (c *Config) HTTPSBase() bool {
	return strings.HasPrefix(c.BaseURL, "https://")
}

// LinkBase is the origin used when building absolute links: base_url when set,
// otherwise http:// plus the listen address, with wildcard hosts as localhost.
func (c *Config) LinkBase() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return "http://" + c.Listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "localhost"
	}
	host = strings.ReplaceAll(host, "%", "%25") // IPv6 zone, as a URL needs it
	return "http://" + net.JoinHostPort(host, port)
}

// ValidateTarget checks the fields shared by config targets and targets
// created from the web UI.
func ValidateTarget(name, rawURL string, interval, timeout time.Duration, expectStatus int) error {
	if name == "" {
		return errors.New("target name is required")
	}
	if len(name) > 64 {
		return errors.New("target name must be 64 characters or fewer")
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("target name must not contain control characters")
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("target %q has invalid url %q", name, rawURL)
	}
	if interval < time.Second {
		return fmt.Errorf("target %q interval must be at least 1s", name)
	}
	if timeout < 100*time.Millisecond {
		return fmt.Errorf("target %q timeout must be at least 100ms", name)
	}
	if timeout > interval {
		return fmt.Errorf("target %q timeout %s exceeds interval %s", name, timeout, interval)
	}
	if expectStatus != 0 && (expectStatus < 100 || expectStatus > 599) {
		return fmt.Errorf("target %q has invalid expect_status %d", name, expectStatus)
	}
	return nil
}

func (c *Config) applyDefaults() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DB == "" {
		c.DB = "uptime.db"
	}
	if c.Interval.Duration <= 0 {
		c.Interval.Duration = 30 * time.Second
	}
	if c.Timeout.Duration <= 0 {
		c.Timeout.Duration = 10 * time.Second
	}
	if c.Retention.Duration <= 0 {
		c.Retention.Duration = 7 * 24 * time.Hour
	}
	// The 24h uptime figures, latency percentiles and hourly bars read raw checks.
	if c.Retention.Duration < 24*time.Hour {
		return fmt.Errorf("config: retention %s is too short; use at least 24h", c.Retention.Duration)
	}
	switch c.AllowPrivateTargets {
	case "":
		c.AllowPrivateTargets = "operator"
	case "none", "operator", "all":
	default:
		return errors.New(`config: allow_private_targets must be "none", "operator" or "all"`)
	}
	if c.BaseURL != "" {
		base, ok := normalizeBaseURL(c.BaseURL)
		if !ok {
			return errors.New("config: base_url must be an absolute http(s) URL with no path")
		}
		c.BaseURL = base
	}
	if c.Signup == "" {
		c.Signup = "closed"
	}
	if c.Signup != "closed" && c.Signup != "invite" {
		return fmt.Errorf("config: signup %q is not supported yet", c.Signup)
	}
	if isSet(c.Mail) {
		return errors.New("config: mail is not supported yet")
	}
	if c.Signup != "closed" {
		if c.BaseURL == "" {
			return fmt.Errorf("config: signup %q needs base_url", c.Signup)
		}
		if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme == "http" && !isLoopbackHost(u.Hostname())) {
			return fmt.Errorf("config: signup %q needs an https base_url (http is allowed only for localhost or a loopback address)", c.Signup)
		}
	}
	if err := c.Limits.applyDefaults(); err != nil {
		return err
	}
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 2
	}
	seen := make(map[string]bool, len(c.Targets))
	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Name == "" {
			return fmt.Errorf("config: target %d has no name", i)
		}
		if seen[t.Name] {
			return fmt.Errorf("config: duplicate target name %q", t.Name)
		}
		seen[t.Name] = true
		if t.Interval.Duration <= 0 {
			t.Interval = c.Interval
		}
		if t.Timeout.Duration <= 0 {
			t.Timeout = c.Timeout
		}
		if err := ValidateTarget(t.Name, t.URL, t.Interval.Duration, t.Timeout.Duration, t.ExpectStatus); err != nil {
			return fmt.Errorf("config: %w", err)
		}
	}
	c.Notify.dropEmpty()
	if err := ValidateNotify(&c.Notify); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// dropEmpty removes channels whose fields are all blank, which is what a
// config like {"bot_token": "${TELEGRAM_BOT_TOKEN}"} becomes when the
// variables are unset. Such a channel is simply not configured.
func (n *Notify) dropEmpty() {
	if t := n.Telegram; t != nil && t.BotToken == "" && t.ChatID == "" {
		n.Telegram = nil
	}
	if s := n.Slack; s != nil && s.WebhookURL == "" {
		n.Slack = nil
	}
	if e := n.Email; e != nil {
		// "to": ["${OPS_EMAIL}"] with the variable unset leaves an empty entry.
		to := e.To[:0]
		for _, a := range e.To {
			if a = strings.TrimSpace(a); a != "" {
				to = append(to, a)
			}
		}
		e.To = to
		if e.SMTPHost == "" && e.From == "" && len(e.To) == 0 && e.Username == "" && e.Password == "" {
			n.Email = nil
		}
	}
}

// ValidateNotify checks alert channel settings and fills in defaults.
func ValidateNotify(n *Notify) error {
	if e := n.Email; e != nil {
		if e.SMTPHost == "" || e.From == "" || len(e.To) == 0 {
			return errors.New("email notifier needs smtp_host, from and to")
		}
		if e.SMTPPort == 0 {
			e.SMTPPort = 587
		}
		if e.SMTPPort < 1 || e.SMTPPort > 65535 {
			return fmt.Errorf("email notifier has invalid port %d", e.SMTPPort)
		}
	}
	if t := n.Telegram; t != nil && (t.BotToken == "" || t.ChatID == "") {
		return errors.New("telegram notifier needs bot_token and chat_id")
	}
	if s := n.Slack; s != nil {
		u, err := url.Parse(s.WebhookURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("slack notifier needs an https webhook_url")
		}
	}
	return nil
}

var telegramTokenRE = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]{30,}$`)

// ValidateNotifyRestricted is ValidateNotify plus the limits for
// non-operator workspaces: a well-formed Telegram token, SMTP on 465 or 587
// with a login, and at most 5 recipients.
func ValidateNotifyRestricted(n *Notify) error {
	if err := ValidateNotify(n); err != nil {
		return err
	}
	if t := n.Telegram; t != nil && !telegramTokenRE.MatchString(t.BotToken) {
		return errors.New("telegram bot_token is not a valid bot token (expected 123456:ABC...)")
	}
	if e := n.Email; e != nil {
		if e.SMTPPort != 465 && e.SMTPPort != 587 {
			return errors.New("email smtp_port must be 465 or 587")
		}
		if e.Username == "" {
			return errors.New("email username is required")
		}
		if len(e.To) > 5 {
			return errors.New("email to may list at most 5 addresses")
		}
	}
	return nil
}

// isSet reports whether an optional raw JSON key was given (and not null).
func isSet(m json.RawMessage) bool {
	return len(m) > 0 && string(m) != "null"
}

// normalizeBaseURL strictly validates a base_url and returns scheme://host
// with the scheme lowercased and any trailing slash dropped.
func normalizeBaseURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || strings.ContainsAny(raw, "?#") || u.User != nil || u.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" || (u.Path != "" && u.Path != "/") {
		return "", false
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", false
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return "", false
		}
	}
	return scheme + "://" + u.Host, true
}

// isLoopbackHost reports whether host is localhost or a loopback IP.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (l *Limits) applyDefaults() error {
	if l.MailPerHour != nil {
		return errors.New("config: limits.mail_per_hour is not supported yet")
	}
	defInt := func(name string, p **int, def int) error {
		if *p == nil {
			*p = &def
		} else if **p < 0 {
			return fmt.Errorf("config: limits.%s must not be negative", name)
		}
		return nil
	}
	if err := defInt("max_monitors", &l.MaxMonitors, 20); err != nil {
		return err
	}
	if err := defInt("max_workspaces", &l.MaxWorkspaces, 0); err != nil {
		return err
	}
	if l.SignupsPerHour == nil {
		l.SignupsPerHour = new(int)
		*l.SignupsPerHour = 100
	}
	if *l.SignupsPerHour < 1 {
		return errors.New("config: limits.signups_per_hour must be at least 1 (to stop new accounts, set limits.max_workspaces to the current workspace count)")
	}
	if l.HostRequestsPerSecond == nil {
		one := 1.0
		l.HostRequestsPerSecond = &one
	} else if *l.HostRequestsPerSecond <= 0 {
		return errors.New("config: limits.host_requests_per_second must be positive")
	}
	if l.MinInterval.Duration == 0 {
		l.MinInterval.Duration = 30 * time.Second
	} else if l.MinInterval.Duration < time.Second {
		return errors.New("config: limits.min_interval must be at least 1s")
	}
	// Mail is not supported yet, so its limit is fixed at the default.
	mail := 500
	l.MailPerHour = &mail
	return nil
}
