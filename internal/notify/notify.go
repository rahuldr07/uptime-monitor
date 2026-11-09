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
	neturl "net/url"
	"strconv"
	"strings"
	"time"

	"uptimemon/internal/config"
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
}

// emailTimeout bounds one delivery attempt when the caller's context has no
// deadline, so a stalled SMTP server cannot hang the alert forever.
const emailTimeout = 30 * time.Second

func (e Email) Notify(ctx context.Context, ev Event) error {
	subject := "[uptime] " + ev.Target + " is "
	if ev.Up {
		subject += "UP"
	} else {
		subject += "DOWN"
	}
	msg := strings.Join([]string{
		"From: " + headerValue(e.From),
		"To: " + headerValue(strings.Join(e.To, ", ")),
		"Subject: " + mime.QEncoding.Encode("utf-8", headerValue(subject)),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: " + messageID(e.From),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		ev.Message(),
		"At: " + ev.At.UTC().Format(time.RFC3339),
		"",
	}, "\r\n")
	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, e.Host)
	}
	var err error
	if e.SendMail != nil {
		err = e.SendMail(net.JoinHostPort(e.Host, strconv.Itoa(e.Port)), auth, e.From, e.To, []byte(msg))
	} else {
		err = e.send(ctx, auth, []byte(msg))
	}
	if err != nil {
		return fmt.Errorf("email: %w", err)
	}
	return nil
}

func (e Email) send(ctx context.Context, auth smtp.Auth, msg []byte) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, emailTimeout)
		defer cancel()
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(e.Host, strconv.Itoa(e.Port)))
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
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
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

// FromConfig builds the configured notifiers. With none configured it
// returns a single Log channel so alerts are never silently dropped.
func FromConfig(cfg config.Notify, logger *slog.Logger) Multi {
	client := &http.Client{Timeout: 15 * time.Second}
	var m Multi
	if t := cfg.Telegram; t != nil && t.BotToken != "" && !t.Disabled {
		m = append(m, Named{"telegram", Telegram{Token: t.BotToken, ChatID: t.ChatID, Client: client}})
	}
	if s := cfg.Slack; s != nil && s.WebhookURL != "" && !s.Disabled {
		m = append(m, Named{"slack", Slack{WebhookURL: s.WebhookURL, Client: client}})
	}
	if e := cfg.Email; e != nil && e.SMTPHost != "" && !e.Disabled {
		m = append(m, Named{"email", Email{Host: e.SMTPHost, Port: e.SMTPPort, Username: e.Username,
			Password: e.Password, From: e.From, To: e.To}})
	}
	if len(m) == 0 {
		m = append(m, Named{"log", Log{Logger: logger}})
	}
	return m
}

func postJSON(ctx context.Context, client *http.Client, url string, body any, name string) error {
	if client == nil {
		client = http.DefaultClient
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
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("%s: unexpected status %d: %s", name, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
