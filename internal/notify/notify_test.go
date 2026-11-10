package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"testing"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/netguard"
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

// fakeSMTP runs a minimal SMTP server that accepts one message and returns
// what was received on the channel.
func fakeSMTP(t *testing.T) (port int, got <-chan string) {
	port, got, _ = fakeSMTPAt(t, "127.0.0.1", "250 8BITMIME")
	return port, got
}

// fakeSMTPAt listens on host, answers EHLO with the given final extension
// line, and records every command it receives.
func fakeSMTPAt(t *testing.T, host, ext string) (port int, got <-chan string, cmds func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	cmds = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		if host != "127.0.0.1" {
			// e.g. macOS only configures 127.0.0.1 on the loopback interface.
			t.Skipf("cannot listen on %s: %v", host, err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprint(conn, "220 fake ESMTP\r\n")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					fmt.Fprint(conn, "250 queued\r\n")
					continue
				}
				data.WriteString(line)
				continue
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			mu.Lock()
			seen = append(seen, cmd)
			mu.Unlock()
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				fmt.Fprint(conn, "250-fake\r\n"+ext+"\r\n")
			case cmd == "DATA":
				inData = true
				fmt.Fprint(conn, "354 go ahead\r\n")
			case cmd == "QUIT":
				fmt.Fprint(conn, "221 bye\r\n")
				ch <- data.String()
				return
			default:
				fmt.Fprint(conn, "250 ok\r\n")
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, ch, cmds
}

func TestEmailDeliversOverSMTP(t *testing.T) {
	port, got := fakeSMTP(t)
	e := Email{Host: "127.0.0.1", Port: port, From: "mon@example.com", To: []string{"ops@example.com"}}
	if err := e.Notify(context.Background(), Event{Target: "api ✓", URL: "https://a", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	msg := <-got
	for _, want := range []string{"Subject: =?utf-8?q?", "Date: ", "Message-ID: <", "@example.com>", "DOWN: api"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestEmailRefusesPlaintextAuth(t *testing.T) {
	// The server offers AUTH but not STARTTLS. 127.0.0.2 is not one of the
	// names net/smtp treats as localhost, so this is a "remote" server and
	// credentials must not be sent over the unencrypted connection.
	port, _, cmds := fakeSMTPAt(t, "127.0.0.2", "250 AUTH PLAIN LOGIN")
	e := Email{Host: "127.0.0.2", Port: port, Username: "u", Password: "secret", From: "a@b", To: []string{"c@d"}}
	if err := e.Notify(context.Background(), Event{Target: "x"}); err == nil {
		t.Fatal("expected an error instead of sending credentials in plain text")
	}
	for _, c := range cmds() {
		if strings.HasPrefix(c, "AUTH") {
			t.Fatalf("credentials were sent without TLS: %q", c)
		}
	}
	if len(cmds()) == 0 {
		t.Fatal("the client never reached the server; the test proves nothing")
	}
}

func TestEmailTimesOut(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(5 * time.Second) // never greets
		}
	}()
	e := Email{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, From: "a@b", To: []string{"c@d"}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := e.Notify(ctx, Event{Target: "x"}); err == nil {
		t.Fatal("expected a timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stalled server should be abandoned at the deadline, took %s", d)
	}
}

func TestEmailHeaderInjection(t *testing.T) {
	var gotMsg string
	e := Email{Host: "h", Port: 25, From: "a@b", To: []string{"c@d"},
		SendMail: func(_ string, _ smtp.Auth, _ string, _ []string, msg []byte) error { gotMsg = string(msg); return nil }}
	e.Notify(context.Background(), Event{Target: "x\r\nBcc: evil@example.com"})
	headers, _, _ := strings.Cut(gotMsg, "\r\n\r\n")
	if strings.Contains(headers, "\r\nBcc:") {
		t.Fatalf("header injected:\n%s", gotMsg)
	}
}

func TestMultiJoinsErrorsAndFromConfig(t *testing.T) {
	failing := Named{"a", notifierFunc(func() error { return errors.New("boom") })}
	okN := Named{"b", notifierFunc(func() error { return nil })}
	err := Multi{failing, okN}.Notify(context.Background(), Event{})
	if err == nil || !strings.Contains(err.Error(), "a: boom") {
		t.Errorf("multi error: %v", err)
	}
	m := FromConfig(config.Notify{}, slog.Default(), true)
	if len(m) != 1 || m[0].Name != "log" {
		t.Errorf("empty config should give log notifier: %+v", m)
	}
	m = FromConfig(config.Notify{Telegram: &config.Telegram{BotToken: "t", ChatID: "c"}, Slack: &config.Slack{WebhookURL: "https://h"}}, nil, true)
	if len(m) != 2 || m[0].Name != "telegram" || m[1].Name != "slack" {
		t.Errorf("from config: %+v", m)
	}
}

type notifierFunc func() error

func (f notifierFunc) Notify(context.Context, Event) error { return f() }

func TestNotifiersGuarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ctx := context.Background()
	cfg := config.Notify{Slack: &config.Slack{WebhookURL: srv.URL}}

	err := FromConfig(cfg, slog.Default(), false).Notify(ctx, Event{Target: "x"})
	if !errors.Is(err, netguard.ErrBlocked) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("slack guarded: %v", err)
	}
	if err := FromConfig(cfg, slog.Default(), true).Notify(ctx, Event{Target: "x"}); err != nil {
		t.Fatalf("slack allowed: %v", err)
	}

	port, _, _ := fakeSMTPAt(t, "127.0.0.1", "250 8BITMIME")
	mail := config.Notify{Email: &config.Email{SMTPHost: "127.0.0.1", SMTPPort: port,
		From: "a@b.c", To: []string{"d@e.f"}}}
	err = FromConfig(mail, slog.Default(), false).Notify(ctx, Event{Target: "x"})
	if !errors.Is(err, netguard.ErrBlocked) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("email guarded: %v", err)
	}
}
