// Package notify delivers up/down alerts to Telegram, Slack and email.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/smtp"
	neturl "net/url"
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

// Email sends plain-text mail over SMTP with STARTTLS or plain auth.
type Email struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
	// SendMail allows tests to stub delivery. Defaults to smtp.SendMail.
	SendMail func(addr string, a smtp.Auth, from string, to []string, msg []byte) error
}

func (e Email) Notify(_ context.Context, ev Event) error {
	subject := "[uptime] " + ev.Target + " is "
	if ev.Up {
		subject += "UP"
	} else {
		subject += "DOWN"
	}
	msg := strings.Join([]string{
		"From: " + e.From,
		"To: " + strings.Join(e.To, ", "),
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		ev.Message(),
		"At: " + ev.At.UTC().Format(time.RFC3339),
		"",
	}, "\r\n")
	var auth smtp.Auth
	if e.Username != "" {
		auth = smtp.PlainAuth("", e.Username, e.Password, e.Host)
	}
	send := e.SendMail
	if send == nil {
		send = smtp.SendMail
	}
	if err := send(fmt.Sprintf("%s:%d", e.Host, e.Port), auth, e.From, e.To, []byte(msg)); err != nil {
		return fmt.Errorf("email: %w", err)
	}
	return nil
}

// FromConfig builds the configured notifiers. With none configured it
// returns a single Log channel so alerts are never silently dropped.
func FromConfig(cfg config.Notify, logger *slog.Logger) Multi {
	client := &http.Client{Timeout: 15 * time.Second}
	var m Multi
	if t := cfg.Telegram; t != nil && t.BotToken != "" {
		m = append(m, Named{"telegram", Telegram{Token: t.BotToken, ChatID: t.ChatID, Client: client}})
	}
	if s := cfg.Slack; s != nil && s.WebhookURL != "" {
		m = append(m, Named{"slack", Slack{WebhookURL: s.WebhookURL, Client: client}})
	}
	if e := cfg.Email; e != nil && e.SMTPHost != "" {
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
