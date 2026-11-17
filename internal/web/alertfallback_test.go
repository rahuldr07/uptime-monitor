package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/mail"
	"github.com/rahuldr07/uptime-monitor/internal/notify"
	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

type sinkSender struct {
	mu sync.Mutex
	to [][]string
}

func (s *sinkSender) Send(_ context.Context, to []string, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.to = append(s.to, to)
	return nil
}

func (s *sinkSender) sent() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.to...)
}

// fallbackEnv is an env whose server has system mail that records deliveries.
func fallbackEnv(t *testing.T) (*env, *mail.Queue, *sinkSender) {
	t.Helper()
	snd := &sinkSender{}
	q := mail.New(mail.Options{Sender: snd, PerHour: 1000})
	e := buildEnv(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}), `{}`, false, func(o *Options) { o.AlertMail = q })
	return e, q, snd
}

func TestFallbackLineAndTest(t *testing.T) {
	e, _, snd := fallbackEnv(t)
	_, body := e.get(t, e.op, "/alerts")
	for _, want := range []string{"No SMTP set up — alerts are sent from the server to " + opEmail, "Send alerts to my account email", `name="server_email" value="1" checked`} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	// The Test button saves the form and tests the fallback.
	resp, body := e.post(t, e.op, "/alerts/test", url.Values{"channel": {"server-email"}, "server_email_form": {"1"}, "server_email": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("test: %d %s", resp.StatusCode, body)
	}
	got := snd.sent()
	if len(got) != 1 || strings.Join(got[0], ",") != opEmail {
		t.Fatalf("delivered to %v, want only %s", got, opEmail)
	}
}

func TestFallbackHiddenWithOwnSMTPOrNoServerMail(t *testing.T) {
	e, _, snd := fallbackEnv(t)
	e.post(t, e.op, "/alerts", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "email_enabled": {"1"}})
	_, body := e.get(t, e.op, "/alerts")
	if strings.Contains(body, "No SMTP set up") {
		t.Fatal("fallback line shown with own SMTP")
	}
	names := map[string]bool{}
	for _, n := range e.mon.Notifiers(context.Background(), 1) {
		names[n.Name] = true
	}
	if names[notify.ServerEmailName] || !names["email"] {
		t.Fatalf("channels = %v", names)
	}
	own := url.Values{"channel": {"server-email"}, "smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "email_enabled": {"1"}}
	if resp, _ := e.post(t, e.op, "/alerts/test", own); resp.StatusCode != http.StatusBadGateway || len(snd.sent()) != 0 {
		t.Fatalf("fallback test with own SMTP: %d", resp.StatusCode)
	}
	// A disabled own channel means email is off: no fallback either.
	e.post(t, e.op, "/alerts", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}})
	for _, n := range e.mon.Notifiers(context.Background(), 1) {
		if n.Name == notify.ServerEmailName {
			t.Fatal("fallback active beside a disabled email channel")
		}
	}
	// No system mail: no line, no channel.
	plain := newEnv(t)
	if _, body := plain.get(t, plain.op, "/alerts"); strings.Contains(body, "No SMTP set up") {
		t.Fatal("fallback line shown without system mail")
	}
}

func TestFallbackSwitchPersistsAcrossOtherSaves(t *testing.T) {
	e, _, snd := fallbackEnv(t)
	ctx := context.Background()
	off := func() bool { n, _ := e.opWS.Notify(ctx); return n.ServerEmailOff }
	active := func() bool {
		for _, n := range e.mon.Notifiers(ctx, 1) {
			if n.Name == notify.ServerEmailName {
				return true
			}
		}
		return false
	}
	e.post(t, e.op, "/alerts", url.Values{"server_email_form": {"1"}}) // box unticked
	if !off() || active() {
		t.Fatalf("switch off: stored %v, active %v", off(), active())
	}
	if _, body := e.get(t, e.op, "/alerts"); strings.Contains(body, `name="server_email" value="1" checked`) || !strings.Contains(body, "Send alerts to my account email") {
		t.Fatal("box should show unticked")
	}
	// Saving Telegram from a form without the switch (as when SMTP is set) keeps it off.
	e.post(t, e.op, "/alerts", url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}})
	if !off() || active() {
		t.Fatal("saving another channel reset the switch")
	}
	e.post(t, e.op, "/alerts", url.Values{"server_email_form": {"1"}, "server_email": {"1"}})
	if off() || !active() {
		t.Fatal("ticking the box should turn it back on")
	}
	e.post(t, e.op, "/alerts", url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}})
	if off() || !active() {
		t.Fatal("saving another channel turned the switch off")
	}
	if len(snd.sent()) != 0 {
		t.Fatal("saving sent mail")
	}
}

func TestFallbackLimitReachedStatus(t *testing.T) {
	e, q, snd := fallbackEnv(t)
	for i := 0; i < 10; i++ {
		q.Admit(1)
	}
	resp, body := e.post(t, e.op, "/alerts/test", url.Values{"channel": {"server-email"}})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "Limit reached") || len(snd.sent()) != 0 {
		t.Fatalf("test over budget: %d sent=%v", resp.StatusCode, snd.sent())
	}
	// The status a dropped alert leaves behind shows on the page.
	if err := e.st.InsertAlert(context.Background(), store.Alert{TargetID: alertTarget(t, e), SentAt: e.now(), Channel: notify.ServerEmailName, Event: "down", Error: "Limit reached"}); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get(t, e.op, "/alerts"); !strings.Contains(body, ">Limit reached</span>") {
		t.Fatal("status badge should read Limit reached")
	}
}

func TestLoginRejectedStatusShown(t *testing.T) {
	e := newEnv(t)
	e.post(t, e.op, "/alerts", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "email_enabled": {"1"}})
	if err := e.st.InsertAlert(context.Background(), store.Alert{TargetID: alertTarget(t, e), SentAt: e.now(), Channel: "email", Event: "down", Error: notify.LoginRejected}); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get(t, e.op, "/alerts"); !strings.Contains(body, "Login rejected — check the username and password") {
		t.Fatal("login rejected message missing")
	}
}

func TestSMTPUserTrimmedPasswordKept(t *testing.T) {
	e := newEnv(t)
	e.post(t, e.op, "/alerts", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"},
		"smtp_user": {"  me@b.co \t"}, "smtp_pass": {" pa ss "}, "email_enabled": {"1"}})
	n, _ := e.opWS.Notify(context.Background())
	if n.Email == nil || n.Email.Username != "me@b.co" || n.Email.Password != " pa ss " {
		t.Fatalf("stored %+v", n.Email)
	}
	// A blank password keeps the saved one.
	e.post(t, e.op, "/alerts", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "smtp_user": {"me@b.co"}, "email_enabled": {"1"}})
	if n, _ = e.opWS.Notify(context.Background()); n.Email.Password != " pa ss " {
		t.Fatalf("password not kept: %q", n.Email.Password)
	}
}

// alertTarget makes a target so an inserted alert belongs to the workspace.
func alertTarget(t *testing.T, e *env) int64 {
	t.Helper()
	tg, err := e.opWS.CreateTarget(context.Background(), store.Target{Name: "api", URL: "https://example.com", Interval: time.Minute, Timeout: time.Second}, e.now())
	if err != nil {
		t.Fatal(err)
	}
	return tg.ID
}
