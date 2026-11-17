// Package notify delivers up/down alerts to Telegram, Slack and email.
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/config"
	"github.com/rahuldr07/uptime-monitor/internal/netguard"
)

// Event describes a state change for a target.
type Event struct {
	Target     string
	URL        string
	Up         bool
	At         time.Time
	StatusCode int
	Error      string
	DownFor    time.Duration // set on recovery
	// Loc is the zone email shows the time in (the workspace owner's); nil means UTC.
	Loc *time.Location
}

// FormatTime renders t in loc (nil means UTC) for people to read outside the
// app, with the offset so it cannot be misread: "2026-11-17 16:30 IST
// (UTC+05:30)", "2026-11-17 11:00 UTC", or "2026-11-17 14:00 UTC+03:00" where
// the zone's abbreviation is only a number.
func FormatTime(t time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	abbr := t.Format("MST")
	date := t.Format("2006-01-02 15:04")
	switch {
	case abbr == "UTC":
		return date + " UTC"
	case abbr[0] == '+' || abbr[0] == '-':
		return date + " " + UTCOffset(t)
	}
	return date + " " + abbr + " (" + UTCOffset(t) + ")"
}

// UTCOffset renders the offset of t's zone from UTC as "UTC+05:30", with an
// ASCII hyphen when negative.
func UTCOffset(t time.Time) string {
	_, off := t.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, off/3600, off%3600/60)
}

// Message renders a one-line human-readable alert.
func (e Event) Message() string {
	if e.Up {
		if e.DownFor > 0 {
			return fmt.Sprintf("🟢 UP: %s (%s) recovered after %s", e.Target, e.URL, e.DownFor.Round(time.Second))
		}
		return fmt.Sprintf("🟢 UP: %s (%s)", e.Target, e.URL)
	}
	reason := e.Error
	if reason == "" && e.StatusCode != 0 {
		reason = fmt.Sprintf("HTTP %d", e.StatusCode)
	}
	if reason == "" {
		reason = "unknown error"
	}
	return fmt.Sprintf("🔴 DOWN: %s (%s) — %s", e.Target, e.URL, reason)
}

// Notifier sends an alert.
type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Named pairs a notifier with the channel name shown in the UI.
type Named struct {
	Name string
	Notifier
}

// Multi fans an event out to every notifier and joins their errors.
type Multi []Named

func (m Multi) Notify(ctx context.Context, ev Event) error {
	var errs []error
	for _, n := range m {
		if err := n.Notify(ctx, ev); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Log writes alerts to the logger. Used when no channel is configured.
type Log struct{ Logger *slog.Logger }

func (l Log) Notify(_ context.Context, ev Event) error {
	l.Logger.Warn("alert", "message", ev.Message())
	return nil
}

// Telegram posts via the Bot API.
type Telegram struct {
	Token   string
	ChatID  string
	BaseURL string // defaults to https://api.telegram.org
	Client  *http.Client
}

func (t Telegram) Notify(ctx context.Context, ev Event) error {
	base := t.BaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	body := map[string]string{"chat_id": t.ChatID, "text": ev.Message()}
	return postJSON(ctx, t.Client, base+"/bot"+t.Token+"/sendMessage", body, "telegram")
}

// Slack posts to an incoming webhook.
type Slack struct {
	WebhookURL string
	Client     *http.Client
}

func (s Slack) Notify(ctx context.Context, ev Event) error {
	return postJSON(ctx, s.Client, s.WebhookURL, map[string]string{"text": ev.Message()}, "slack")
}

// Email sends plain-text mail over SMTP. Port 465 uses implicit TLS; any
// other port upgrades with STARTTLS when the server offers it. Credentials
// are never sent over an unencrypted connection to a remote host.
type Email struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	// SendMail allows tests to stub delivery. When nil, mail is sent with a
	// context-aware SMTP client.
	SendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
	// TLSConfig overrides the TLS settings, for tests.
	TLSConfig *tls.Config
	// RequireTLS refuses to send unless the connection is encrypted: on a
	// port other than 465 the server must offer STARTTLS.
	RequireTLS bool
	// Dial opens the connection. Nil means an unguarded dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// emailTimeout bounds one delivery attempt when the caller's context has no
// deadline, so a stalled SMTP server cannot hang the alert forever.
const emailTimeout = 30 * time.Second

func (e Email) Notify(ctx context.Context, ev Event) error {
	subject, body := emailMessage(ev)
	return e.Send(ctx, e.To, subject, body)
}

// emailMessage renders the subject and body of an alert email.
func emailMessage(ev Event) (subject, body string) {
	subject = "[uptime] " + ev.Target + " is "
	if ev.Up {
		subject += "UP"
	} else {
		subject += "DOWN"
	}
	return subject, ev.Message() + "\r\nAt: " + FormatTime(ev.At, ev.Loc) + "\r\n"
}

// ErrLimit means an alert email was dropped because a budget is spent.
var ErrLimit = errors.New(LimitReached)

// ErrServerMail wraps any failure of the server's own mail path. Its text is
// all a tenant sees; the wrapped error is for the log only, as it may name
// the operator's SMTP host or account.
var ErrServerMail = errors.New(ServerMailFailed)

// ErrNoRecipient means no member has a verified address to mail.
var ErrNoRecipient = errors.New("no verified account email")

// Status texts shown on the Alert channels page.
const (
	LimitReached     = "Limit reached"
	ServerMailFailed = "Server mail failed"
)

// LoginRejected is the status shown when an SMTP server refuses the login.
const LoginRejected = "Login rejected — check the username and password"

// StatusText is the short status shown for a failed delivery: friendly text
// for a spent budget or a refused SMTP login, the error itself otherwise. The
// raw error stays in the log.
func StatusText(err error) string {
	var te *textproto.Error
	switch {
	case errors.Is(err, ErrLimit):
		return LimitReached
	case errors.Is(err, ErrServerMail):
		return ServerMailFailed
	case errors.Is(err, ErrNoRecipient):
		return ErrNoRecipient.Error()
	case errors.As(err, &te) && (te.Code == 530 || te.Code == 534 || te.Code == 535 || te.Code == 538):
		return LoginRejected
	}
	return err.Error()
}

// Admitter is implemented by a notifier whose use is budgeted. Admit is
// called once per alert, before delivery, so retries are not charged again.
type Admitter interface {
	Admit(ctx context.Context) error
}

// SystemMail is the server's own mail path (internal/mail.Queue).
type SystemMail interface {
	// Admit charges one alert email (with its recipient count) to the
	// workspace and instance budgets and returns why it was refused, or "" when it may be sent.
	Admit(workspace int64, recipients int) (reason string)
	Deliver(ctx context.Context, to []string, subject, body string) error
}

// ServerEmailName is the channel name of the fallback alert email.
const ServerEmailName = "server-email"

// ServerEmail sends alert email through the server's system mail to the
// workspace's verified member addresses, never to an address a user typed.
type ServerEmail struct {
	Workspace  int64
	Mail       SystemMail
	Recipients func(ctx context.Context) ([]string, error)
}

// Admit refuses when nobody can receive the alert or a budget is spent.
func (s ServerEmail) Admit(ctx context.Context) error {
	to, err := s.recipients(ctx)
	if err != nil {
		return err
	}
	if reason := s.Mail.Admit(s.Workspace, len(to)); reason != "" {
		return fmt.Errorf("%w (%s)", ErrLimit, reason)
	}
	return nil
}

func (s ServerEmail) recipients(ctx context.Context) ([]string, error) {
	to, err := s.Recipients(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrServerMail, err)
	}
	if len(to) == 0 {
		return nil, ErrNoRecipient
	}
	return to, nil
}

func (s ServerEmail) Notify(ctx context.Context, ev Event) error {
	to, err := s.recipients(ctx)
	if err != nil {
		return err
	}
	subject, body := emailMessage(ev)
	if err := s.Mail.Deliver(ctx, to, subject, body); err != nil {
		return fmt.Errorf("%w: %v", ErrServerMail, err)
	}
	return nil
}

// WithServerEmail adds the fallback alert email to m when the server has mail
// (sm non-nil), the workspace has no email channel (no SMTP host; a disabled
// channel with a host counts as set up) and the switch is not off.
func WithServerEmail(m Multi, cfg config.Notify, sm SystemMail, workspace int64, recipients func(ctx context.Context) ([]string, error)) Multi {
	if sm == nil || cfg.ServerEmailOff || (cfg.Email != nil && cfg.Email.SMTPHost != "") {
		return m
	}
	return append(m, Named{ServerEmailName, ServerEmail{Workspace: workspace, Mail: sm, Recipients: recipients}})
}

// Send delivers one plain-text message to the given recipients.
func (e Email) Send(ctx context.Context, to []string, subject, body string) error {
	msg := strings.Join([]string{
		"From: " + headerValue(e.From),
		"To: " + headerValue(strings.Join(to, ", ")),
		"Subject: " + mime.QEncoding.Encode("utf-8", headerValue(subject)),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: " + messageID(e.From),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		body,
	}, "\r\n")
	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, e.Host)
	}
	var err error
	if e.SendMail != nil {
		err = e.SendMail(net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), auth, e.From, to, []byte(msg))
	} else {
		err = e.send(ctx, auth, to, []byte(msg))
	}
	if err != nil {
		if errors.Is(err, netguard.ErrBlocked) {
			return fmt.Errorf("email: %w", netguard.ErrBlocked)
		}
		return fmt.Errorf("email: %w", err)
	}
	return nil
}

func (e Email) send(ctx context.Context, auth smtp.Auth, to []string, msg []byte) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, emailTimeout)
		defer cancel()
	}
	dial := e.Dial
	if dial == nil {
		dial = netguard.Dialer(true, 0).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	// Cancelling the context (e.g. on shutdown) aborts the SMTP conversation.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	tlsCfg := e.TLSConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: e.Host, MinVersion: tls.VersionTLS12}
	}
	if e.Port == 465 {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return err
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		return err
	}
	defer c.Close()
	if e.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return err
			}
		} else if e.RequireTLS {
			return errors.New("server does not offer STARTTLS")
		}
	}
	if auth != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("server does not support authentication")
		}
		if err := c.Auth(auth); err != nil {
			return err
		}
	}
	if err := c.Mail(e.From); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// headerValue strips line breaks so a value cannot inject extra headers.
func headerValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
}

func messageID(from string) string {
	var b [12]byte
	rand.Read(b[:])
	domain := "uptime-monitor"
	if i := strings.LastIndex(from, "@"); i >= 0 && i < len(from)-1 {
		domain = strings.Trim(from[i+1:], "<> \r\n")
	}
	return "<" + hex.EncodeToString(b[:]) + "@" + domain + ">"
}

// FromConfig builds the configured notifiers. With restricted set (non-operator
// workspaces) webhooks do not follow redirects and email requires TLS. With none configured it
// returns a single Log channel so alerts are never silently dropped.
func FromConfig(cfg config.Notify, logger *slog.Logger, allowPrivate, restricted bool) Multi {
	if restricted {
		// Re-check at send time what the save form checked, so a channel
		// stored before a rule tightened (or written to the database
		// directly) is not used.
		cfg = restrictedChannels(cfg, logger)
	}
	return build(cfg, logger, allowPrivate, restricted)
}

// build makes the notifiers for cfg without validating it.
func build(cfg config.Notify, logger *slog.Logger, allowPrivate, restricted bool) Multi {
	client := netguard.Client(allowPrivate, 15*time.Second)
	if restricted {
		// A redirect could carry the request (and its token) elsewhere.
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	var m Multi
	if t := cfg.Telegram; t != nil && t.BotToken != "" && !t.Disabled {
		m = append(m, Named{"telegram", Telegram{Token: t.BotToken, ChatID: t.ChatID, Client: client}})
	}
	if s := cfg.Slack; s != nil && s.WebhookURL != "" && !s.Disabled {
		m = append(m, Named{"slack", Slack{WebhookURL: s.WebhookURL, Client: client}})
	}
	if e := cfg.Email; e != nil && e.SMTPHost != "" && !e.Disabled {
		m = append(m, Named{"email", Email{Host: e.SMTPHost, Port: e.SMTPPort, Username: e.Username,
			Password: e.Password, From: e.From, To: e.To, RequireTLS: restricted,
			Dial: netguard.Dialer(allowPrivate, 30*time.Second).DialContext}})
	}
	if len(m) == 0 {
		m = append(m, Named{"log", Log{Logger: logger}})
	}
	return m
}

// restrictedChannels returns cfg without the channels that fail the
// non-operator validation, logging each one dropped.
func restrictedChannels(cfg config.Notify, logger *slog.Logger) config.Notify {
	drop := func(name string, err error) {
		if logger != nil {
			logger.Error("alert channel dropped: invalid for a non-operator workspace", "channel", name, "err", err)
		}
	}
	if cfg.Telegram != nil {
		if err := config.ValidateNotifyRestricted(&config.Notify{Telegram: cfg.Telegram}); err != nil {
			drop("telegram", err)
			cfg.Telegram = nil
		}
	}
	if cfg.Slack != nil {
		if err := config.ValidateNotifyRestricted(&config.Notify{Slack: cfg.Slack}); err != nil {
			drop("slack", err)
			cfg.Slack = nil
		}
	}
	if cfg.Email != nil {
		if err := config.ValidateNotifyRestricted(&config.Notify{Email: cfg.Email}); err != nil {
			drop("email", err)
			cfg.Email = nil
		}
	}
	return cfg
}

func postJSON(ctx context.Context, client *http.Client, url string, body any, name string) error {
	if client == nil {
		client = netguard.Client(true, 15*time.Second)
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		var ue *neturl.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL, which may carry a token
		}
		if errors.Is(err, netguard.ErrBlocked) {
			return fmt.Errorf("%s: %w", name, netguard.ErrBlocked)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s: unexpected status %d: %s", name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
