package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/config"
)

func TestMessage(t *testing.T) {
	down := Event{Target: "api", URL: "https://a", StatusCode: 503}
	if got := down.Message(); !strings.Contains(got, "DOWN") || !strings.Contains(got, "HTTP 503") {
		t.Errorf("down message: %q", got)
	}
	up := Event{Target: "api", URL: "https://a", Up: true, DownFor: 90 * time.Second}
	if got := up.Message(); !strings.Contains(got, "recovered after 1m30s") {
		t.Errorf("up message: %q", got)
	}
	if got := (Event{Target: "x", Error: "timeout after 10s"}).Message(); !strings.Contains(got, "timeout after 10s") {
		t.Errorf("error message: %q", got)
	}
}

func TestTelegramAndSlack(t *testing.T) {
	var gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		if strings.Contains(r.URL.Path, "fail") {
			http.Error(w, "nope", 500)
		}
	}))
	defer srv.Close()
	ev := Event{Target: "api", URL: "https://a", Up: true}

	tg := Telegram{Token: "tok", ChatID: "42", BaseURL: srv.URL, Client: srv.Client()}
	if err := tg.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bottok/sendMessage" || gotBody["chat_id"] != "42" || !strings.Contains(gotBody["text"], "UP") {
		t.Errorf("telegram request: %s %v", gotPath, gotBody)
	}

	sl := Slack{WebhookURL: srv.URL + "/hook", Client: srv.Client()}
	if err := sl.Notify(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	if gotBody["text"] == "" {
		t.Errorf("slack body: %v", gotBody)
	}

	bad := Slack{WebhookURL: srv.URL + "/fail", Client: srv.Client()}
	if err := bad.Notify(context.Background(), ev); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("expected status error, got %v", err)
	}
}

func TestEmailBuildsMessage(t *testing.T) {
	var gotAddr, gotMsg string
	var gotTo []string
	e := Email{Host: "smtp.test", Port: 2525, Username: "u", Password: "p", From: "a@b", To: []string{"x@y", "z@w"},
		SendMail: func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
			gotAddr, gotTo, gotMsg = addr, to, string(msg)
			if a == nil {
				return errors.New("expected auth")
			}
			return nil
		}}
	if err := e.Notify(context.Background(), Event{Target: "api", URL: "https://a", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "smtp.test:2525" || len(gotTo) != 2 || !strings.Contains(gotMsg, "Subject: [uptime] api is DOWN") {
		t.Errorf("addr=%s to=%v msg=%q", gotAddr, gotTo, gotMsg)
	}
}

func TestMultiJoinsErrorsAndFromConfig(t *testing.T) {
	failing := Named{"a", notifierFunc(func() error { return errors.New("boom") })}
	okN := Named{"b", notifierFunc(func() error { return nil })}
	err := Multi{failing, okN}.Notify(context.Background(), Event{})
	if err == nil || !strings.Contains(err.Error(), "a: boom") {
		t.Errorf("multi error: %v", err)
	}
	m := FromConfig(config.Notify{}, slog.Default())
	if len(m) != 1 || m[0].Name != "log" {
		t.Errorf("empty config should give log notifier: %+v", m)
	}
	m = FromConfig(config.Notify{Telegram: &config.Telegram{BotToken: "t", ChatID: "c"}, Slack: &config.Slack{WebhookURL: "https://h"}}, nil)
	if len(m) != 2 || m[0].Name != "telegram" || m[1].Name != "slack" {
		t.Errorf("from config: %+v", m)
	}
}

type notifierFunc func() error

func (f notifierFunc) Notify(context.Context, Event) error { return f() }
