package web

import (
	"context"
	"errors"
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
	_, body := e.get(t, e.op, "/settings/notifications")
	for _, want := range []string{"Alerts are emailed from this server to " + opEmail + ". To send from your own mail account or to other addresses, fill in SMTP below.", `name="server_email" value="1" checked`, "no deliveries yet"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	// The Test button saves the form and tests the fallback.
	resp, body := e.post(t, e.op, "/settings/notifications/test", url.Values{"channel": {"server-email"}, "server_email_form": {"1"}, "server_email": {"1"}})
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
	e.post(t, e.op, "/settings/notifications", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "email_enabled": {"1"}})
	_, body := e.get(t, e.op, "/settings/notifications")
	if strings.Contains(body, "emailed from this server") {
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
	if resp, _ := e.post(t, e.op, "/settings/notifications/test", own); resp.StatusCode != http.StatusBadGateway || len(snd.sent()) != 0 {
		t.Fatalf("fallback test with own SMTP: %d", resp.StatusCode)
	}
	// A disabled own channel means email is off: no fallback either.
	e.post(t, e.op, "/settings/notifications", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}})
	for _, n := range e.mon.Notifiers(context.Background(), 1) {
		if n.Name == notify.ServerEmailName {
			t.Fatal("fallback active beside a disabled email channel")
		}
	}
	// No system mail: no line, no channel.
	plain := newEnv(t)
	if _, body := plain.get(t, plain.op, "/settings/notifications"); strings.Contains(body, "emailed from this server") {
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
	e.post(t, e.op, "/settings/notifications", url.Values{"server_email_form": {"1"}}) // box unticked
	if !off() || active() {
		t.Fatalf("switch off: stored %v, active %v", off(), active())
	}
	_, body := e.get(t, e.op, "/settings/notifications")
	card := body[strings.Index(body, `name="server_email"`):strings.Index(body, "SMTP host")]
	if strings.Contains(body, `name="server_email" value="1" checked`) || !strings.Contains(body, "Email alerts are off. Tick Email to have this server send them to "+opEmail+".") ||
		strings.Contains(card, "badge") {
		t.Fatal("box should show unticked with the off copy and no status badge")
	}
	// Saving Telegram from a form without the switch (as when SMTP is set) keeps it off.
	e.post(t, e.op, "/settings/notifications", url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}})
	if !off() || active() {
		t.Fatal("saving another channel reset the switch")
	}
	e.post(t, e.op, "/settings/notifications", url.Values{"server_email_form": {"1"}, "server_email": {"1"}})
	if off() || !active() {
		t.Fatal("ticking the box should turn it back on")
	}
	e.post(t, e.op, "/settings/notifications", url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}})
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
		q.Admit(1, 1)
	}
	resp, body := e.post(t, e.op, "/settings/notifications/test", url.Values{"channel": {"server-email"}})
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "Limit reached") || len(snd.sent()) != 0 {
		t.Fatalf("test over budget: %d sent=%v", resp.StatusCode, snd.sent())
	}
	// The status a dropped alert leaves behind shows on the page.
	if err := e.st.InsertAlert(context.Background(), store.Alert{TargetID: alertTarget(t, e), SentAt: e.now(), Channel: notify.ServerEmailName, Event: "down", Error: notify.LimitReached}); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get(t, e.op, "/settings/notifications"); !strings.Contains(body, ">Limit reached</span>") {
		t.Fatal("status badge should read Limit reached")
	}
}

func TestLoginRejectedStatusShown(t *testing.T) {
	e := newEnv(t)
	e.post(t, e.op, "/settings/notifications", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "email_enabled": {"1"}})
	if err := e.st.InsertAlert(context.Background(), store.Alert{TargetID: alertTarget(t, e), SentAt: e.now(), Channel: "email", Event: "down", Error: notify.LoginRejected}); err != nil {
		t.Fatal(err)
	}
	if _, body := e.get(t, e.op, "/settings/notifications"); !strings.Contains(body, "Login rejected — check the username and password") {
		t.Fatal("login rejected message missing")
	}
}

func TestSMTPUserAndPasswordTrimmed(t *testing.T) {
	e := newEnv(t)
	e.post(t, e.op, "/settings/notifications", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"},
		"smtp_user": {"  me@b.co \t"}, "smtp_pass": {" pa ss "}, "email_enabled": {"1"}})
	n, _ := e.opWS.Notify(context.Background())
	if n.Email == nil || n.Email.Username != "me@b.co" || n.Email.Password != "pa ss" {
		t.Fatalf("stored %+v", n.Email)
	}
	// A blank password keeps the saved one.
	e.post(t, e.op, "/settings/notifications", url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "smtp_user": {"me@b.co"}, "email_enabled": {"1"}})
	if n, _ = e.opWS.Notify(context.Background()); n.Email.Password != "pa ss" {
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

func TestUnboundInviteWithMailMustVerifyBeforeAlerts(t *testing.T) {
	snd := &sinkSender{}
	q := mail.New(mail.Options{Sender: snd, PerHour: 1000})
	e := buildEnv(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}),
		`{"signup":"invite","base_url":"http://localhost:8080"}`, true, func(o *Options) { o.AlertMail = q })
	ctx := context.Background()
	raw := e.invite(t, "")
	c := e.client(t)
	resp, body := e.post(t, c, "/signup", signupForm(raw, "new@example.com"))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Check your email") {
		t.Fatalf("unbound invite with mail: %d %s", resp.StatusCode, body)
	}
	u, _, err := e.st.UserByEmail(ctx, "new@example.com")
	if err != nil || u.VerifiedAt != nil {
		t.Fatalf("user %+v err %v: want unverified", u, err)
	}
	if len(e.mail.sent) != 1 || e.mail.sent[0].To != "new@example.com" || linkFrom(e.mail.sent[0].Body) == "" {
		t.Fatalf("verification mail: %+v", e.mail.sent)
	}
	ws := e.st.Workspace(2) // the new account's workspace; the operator's is 1
	if got, _ := ws.MemberEmails(ctx); len(got) != 0 {
		t.Fatalf("an unverified address is a fallback recipient: %v", got)
	}
	se := notify.ServerEmail{Workspace: ws.ID(), Mail: q, Recipients: ws.MemberEmails}
	if err := se.Admit(ctx); !errors.Is(err, notify.ErrNoRecipient) || len(snd.sent()) != 0 {
		t.Fatalf("alert before verification: %v", err)
	}
	// Verifying makes the address a recipient.
	link := linkFrom(e.mail.sent[0].Body)
	if resp, body := e.get(t, e.client(t), strings.TrimPrefix(link, "http://localhost:8080")); resp.StatusCode >= 400 {
		t.Fatalf("verify link: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.post(t, e.client(t), "/verify", url.Values{"token": {link[strings.Index(link, "token=")+6:]}}); resp.StatusCode >= 400 {
		t.Fatalf("verify: %d %s", resp.StatusCode, body)
	}
	if got, _ := ws.MemberEmails(ctx); len(got) != 1 || got[0] != "new@example.com" {
		t.Fatalf("after verifying: %v", got)
	}
}

func TestBoundInviteIsVerified(t *testing.T) {
	e := newEnvMail(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	raw := e.invite(t, "bound@example.com")
	if resp, body := e.post(t, e.client(t), "/signup", signupForm(raw, "bound@example.com")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bound invite: %d %s", resp.StatusCode, body)
	}
	if u, _, err := e.st.UserByEmail(t.Context(), "bound@example.com"); err != nil || u.VerifiedAt == nil {
		t.Fatalf("bound invite should verify: %+v %v", u, err)
	}
	if len(e.mail.sent) != 0 {
		t.Fatalf("a verification mail was sent: %+v", e.mail.sent)
	}
}

func TestUnboundInviteWithoutMailIsVerified(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	raw := e.invite(t, "")
	if resp, body := e.post(t, e.client(t), "/signup", signupForm(raw, "new@example.com")); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("no mail: %d %s", resp.StatusCode, body)
	}
	if u, _, err := e.st.UserByEmail(t.Context(), "new@example.com"); err != nil || u.VerifiedAt == nil {
		t.Fatalf("no mail: want verified, got %+v %v", u, err)
	}
}

func TestFallbackForNonOperatorTenant(t *testing.T) {
	e, _, snd := fallbackEnv(t)
	ctx := context.Background()
	phc, err := e.hash.Hash(ctx, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	_, ws, err := e.st.CreateAccount(ctx, store.AccountInput{Email: "tenant@example.com", PasswordHash: phc, WorkspaceName: "Tenant", Slug: "tenant", Verified: true, Now: e.now()})
	if err != nil {
		t.Fatal(err)
	}
	c := e.signIn(t, "tenant@example.com", testSecret)
	if _, body := e.get(t, c, "/settings/notifications"); !strings.Contains(body, "Alerts are emailed from this server to tenant@example.com") {
		t.Fatal("tenant page lacks the fallback copy")
	}
	resp, body := e.post(t, c, "/settings/notifications/test", url.Values{"channel": {"server-email"}, "server_email_form": {"1"}, "server_email": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("tenant test: %d %s", resp.StatusCode, body)
	}
	if got := snd.sent(); len(got) != 1 || strings.Join(got[0], ",") != "tenant@example.com" {
		t.Fatalf("delivered to %v, want only the tenant", got)
	}
	// The operator's budget and recipient are separate from the tenant's.
	if m, _ := e.opWS.MemberEmails(ctx); strings.Join(m, ",") != opEmail || ws.ID() == 1 {
		t.Fatalf("operator recipients %v", m)
	}
}

func TestFallbackHeaderBoxDrivesOwnSMTPEnabled(t *testing.T) {
	e, _, _ := fallbackEnv(t)
	ctx := context.Background()
	smtp := url.Values{"smtp_host": {"smtp.example.com"}, "smtp_from": {"a@b.co"}, "smtp_to": {"x@y.co"}, "server_email_form": {"1"}}
	smtp.Set("server_email", "1") // Email ticked in the fallback form, with SMTP filled in
	e.post(t, e.op, "/settings/notifications", smtp)
	if n, _ := e.opWS.Notify(ctx); n.Email == nil || n.Email.Disabled {
		t.Fatalf("ticked Email with SMTP should save an enabled channel: %+v", n.Email)
	}
	smtp.Del("server_email")
	e.post(t, e.op, "/settings/notifications", smtp)
	if n, _ := e.opWS.Notify(ctx); n.Email == nil || !n.Email.Disabled {
		t.Fatalf("unticked Email should save a disabled channel: %+v", n.Email)
	}
}
