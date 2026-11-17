package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

type sentMail struct{ To, Subject, Body string }

// fakeMailer records system mail. With drop set it behaves like an exhausted
// budget: compose is not called.
type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	drop bool
}

func (m *fakeMailer) Send(to string, compose func() (string, string, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.drop {
		return nil
	}
	subject, body, err := compose()
	if err != nil {
		return err
	}
	m.sent = append(m.sent, sentMail{to, subject, body})
	return nil
}

func (m *fakeMailer) Notice(to, subject, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentMail{to, subject, body})
}

func (m *fakeMailer) all() []sentMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentMail(nil), m.sent...)
}

const openCfg = `{"signup":"open","base_url":"http://127.0.0.1","mail":{"smtp_host":"x.example","from":"up@example.com"}}`

// newEnvMail is an env with the recording mailer wired in.
func newEnvMail(t *testing.T, cfgJSON string) *env {
	t.Helper()
	return buildEnv(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}), cfgJSON, true)
}

var linkRe = regexp.MustCompile(`https?://[^\s]+\?token=[A-Za-z0-9_-]+`)

// linkFrom extracts the token URL from a mail body.
func linkFrom(body string) string { return linkRe.FindString(body) }

// linkPath is the link's path and query, to request it from the test server.
func linkPath(link string) string {
	u, _ := url.Parse(link)
	return u.RequestURI()
}

func linkToken(link string) string {
	u, _ := url.Parse(link)
	return u.Query().Get("token")
}

// signUpOpen posts an open sign-up from the given client address.
func (e *env) signUpOpen(t *testing.T, ip, email string) (*http.Response, string) {
	t.Helper()
	return e.post(t, e.clientFrom(t, ip), "/signup", signupForm("", email))
}

// addUnverified inserts an unverified account directly.
func (e *env) addUnverified(t *testing.T, email string) {
	t.Helper()
	phc, err := e.hash.Hash(t.Context(), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	local := strings.SplitN(email, "@", 2)[0]
	if _, _, err := e.st.CreateAccount(t.Context(), store.AccountInput{Email: email, PasswordHash: phc,
		WorkspaceName: local, Slug: local, Now: e.now()}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) disable(t *testing.T, email string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+e.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE users SET disabled_at=? WHERE email=?`, e.now().UnixMilli(), email); err != nil {
		t.Fatal(err)
	}
}

func (e *env) verified(t *testing.T, email string) bool {
	t.Helper()
	u, _, err := e.st.UserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.VerifiedAt != nil
}

// openSignedUp signs up email and returns its verification link.
func (e *env) openSignedUp(t *testing.T, ip, email string) string {
	t.Helper()
	if resp, body := e.signUpOpen(t, ip, email); resp.StatusCode != 200 || !strings.Contains(body, msgCheckEmail) {
		t.Fatalf("sign-up: %d %s", resp.StatusCode, body)
	}
	sent := e.mail.all()
	if len(sent) == 0 {
		t.Fatal("no verification mail")
	}
	return linkFrom(sent[len(sent)-1].Body)
}

func TestOpenSignupSendsVerify(t *testing.T) {
	e := newEnvMail(t, openCfg)
	resp, body := e.signUpOpen(t, "127.0.0.2", "new@x.example")
	if resp.StatusCode != 200 || !strings.Contains(body, msgCheckEmail) {
		t.Fatalf("sign-up: %d %s", resp.StatusCode, body)
	}
	if len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("cookie set: %v", resp.Header.Values("Set-Cookie"))
	}
	sent := e.mail.all()
	if len(sent) != 1 || sent[0].To != "new@x.example" || sent[0].Subject != "Confirm your email" ||
		!strings.Contains(sent[0].Body, "/verify?token=") {
		t.Fatalf("mail = %+v", sent)
	}
	resp, body = e.post(t, e.client(t), "/login", url.Values{"email": {"new@x.example"}, "password": {testSecret}})
	if resp.StatusCode != 403 || !strings.Contains(body, msgUnverified) || !strings.Contains(body, `action="/verify/resend"`) {
		t.Fatalf("unverified login: %d %s", resp.StatusCode, body)
	}
}

func TestVerifyGetChangesNothing(t *testing.T) {
	e := newEnvMail(t, openCfg)
	link := e.openSignedUp(t, "127.0.0.2", "new@x.example")
	c := e.client(t)
	for i := 0; i < 2; i++ {
		resp, body := e.get(t, c, linkPath(link))
		if resp.StatusCode != 200 || !strings.Contains(body, "Confirm your email") || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if e.verified(t, "new@x.example") {
		t.Fatal("GET verified the account")
	}
	if resp, body := e.post(t, c, "/verify", url.Values{"token": {linkToken(link)}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("token did not survive the GETs: %d %s", resp.StatusCode, body)
	}
}

func TestVerifyPostSignsIn(t *testing.T) {
	e := newEnvMail(t, openCfg)
	link := e.openSignedUp(t, "127.0.0.2", "new@x.example")
	c := e.client(t)
	resp, body := e.post(t, c, "/verify", url.Values{"token": {linkToken(link)}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || cookieNamed(resp, "uptime_session") == nil {
		t.Fatalf("verify: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if resp, _ := e.get(t, c, "/"); resp.StatusCode != 200 {
		t.Fatalf("session: %d", resp.StatusCode)
	}
	if !e.verified(t, "new@x.example") {
		t.Fatal("not verified")
	}
	resp, body = e.post(t, e.client(t), "/verify", url.Values{"token": {linkToken(link)}})
	if resp.StatusCode != 404 || !strings.Contains(body, msgBadLink) {
		t.Fatalf("second use: %d %s", resp.StatusCode, body)
	}
}

func TestVerifyOtherUsersSession(t *testing.T) {
	e := newEnvMail(t, openCfg)
	link := e.openSignedUp(t, "127.0.0.2", "a@x.example")
	b, _ := e.account(t, "b@x.example")
	resp, body := e.post(t, b, "/verify", url.Values{"token": {linkToken(link)}})
	if resp.StatusCode != 200 || !strings.Contains(body, msgVerifiedSignIn) || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("verify as B: %d %v %s", resp.StatusCode, resp.Header.Values("Set-Cookie"), body)
	}
	if !e.verified(t, "a@x.example") {
		t.Fatal("A not verified")
	}
	if resp, body := e.get(t, b, "/settings/profile"); resp.StatusCode != 200 || !strings.Contains(body, "b@x.example") {
		t.Fatalf("B session: %d", resp.StatusCode)
	}
	e.signIn(t, "a@x.example", testSecret)
}

func TestOpenSignupIdenticalResponses(t *testing.T) {
	e := newEnvMail(t, openCfg)
	norm := func(resp *http.Response, body, email string) string {
		return resp.Status + "|" + strings.Join(resp.Header.Values("Set-Cookie"), ";") + "|" + strings.ReplaceAll(body, email, "EMAIL")
	}
	r1, b1 := e.signUpOpen(t, "127.0.0.2", "new@x.example")
	r2, b2 := e.signUpOpen(t, "127.0.0.3", opEmail)
	if r1.StatusCode != 200 || norm(r1, b1, "new@x.example") != norm(r2, b2, opEmail) {
		t.Fatalf("new vs existing:\n%d %s\n%d %s", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	// Both cases charged the same slots: from each address, the same number
	// of further sign-ups gets through before the first 429.
	until429 := func(ip string, email func(i int) string) int {
		for i := 1; i <= 20; i++ {
			if resp, _ := e.signUpOpen(t, ip, email(i)); resp.StatusCode == 429 {
				return i
			}
		}
		t.Fatal("never throttled")
		return 0
	}
	nNew := until429("127.0.0.2", func(i int) string { return fmt.Sprintf("more%d@x.example", i) })
	nDup := until429("127.0.0.3", func(int) string { return opEmail })
	if nNew != nDup {
		t.Fatalf("limiter slots differ: new throttled at %d, duplicate at %d", nNew, nDup)
	}
	e.advance(2 * time.Hour)
	n := e.userCount(t) // one workspace per user
	e.cfg.Limits.MaxWorkspaces = &n
	r1, b1 = e.signUpOpen(t, "127.0.0.4", "other@x.example")
	r2, b2 = e.signUpOpen(t, "127.0.0.5", opEmail)
	if r1.StatusCode != 403 || !strings.Contains(b1, msgNoNewAccounts) ||
		norm(r1, b1, "other@x.example") != norm(r2, b2, opEmail) {
		t.Fatalf("at cap:\n%d %s\n%d %s", r1.StatusCode, b1, r2.StatusCode, b2)
	}
}

func TestDuplicateNoticeOncePerDay(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.openSignedUp(t, "127.0.0.2", "new@x.example")
	dups := func() (n int) {
		for _, m := range e.mail.all() {
			if m.Subject == "Sign-up attempt with your address" {
				if m.To != "new@x.example" || !strings.Contains(m.Body, "http://127.0.0.1/forgot") {
					t.Fatalf("notice = %+v", m)
				}
				n++
			}
		}
		return n
	}
	e.signUpOpen(t, "127.0.0.3", "new@x.example")
	e.signUpOpen(t, "127.0.0.4", "new@x.example")
	if dups() != 1 {
		t.Fatalf("notices = %d, want 1", dups())
	}
	e.advance(25 * time.Hour)
	e.signUpOpen(t, "127.0.0.5", "new@x.example")
	if dups() != 2 {
		t.Fatalf("notices after a day = %d, want 2", dups())
	}
}

func TestDroppedDuplicateNoticeKeepsSlot(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.openSignedUp(t, "127.0.0.2", "new@x.example")
	before := len(e.mail.all())
	e.mail.mu.Lock()
	e.mail.drop = true
	e.mail.mu.Unlock()
	e.signUpOpen(t, "127.0.0.3", "new@x.example")
	if n := len(e.mail.all()); n != before {
		t.Fatalf("dropped notice sent mail: %d -> %d", before, n)
	}
	e.mail.mu.Lock()
	e.mail.drop = false
	e.mail.mu.Unlock()
	e.signUpOpen(t, "127.0.0.4", "new@x.example")
	if n := len(e.mail.all()); n != before+1 {
		t.Fatalf("notice after drop: %d -> %d, want one more", before, n)
	}
}

func TestResendIdentical(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.addUnverified(t, "unv@x.example")
	e.addUnverified(t, "dis@x.example")
	e.disable(t, "dis@x.example")
	c := e.client(t)
	var first string
	for i, em := range []string{"nobody@x.example", opEmail, "unv@x.example", "dis@x.example"} {
		resp, body := e.post(t, c, "/verify/resend", url.Values{"email": {em}})
		if resp.StatusCode != 200 || !strings.Contains(body, msgResendSent) || strings.Contains(body, em) {
			t.Fatalf("%s: %d %s", em, resp.StatusCode, body)
		}
		if i == 0 {
			first = body
		} else if body != first {
			t.Fatalf("%s: body differs", em)
		}
	}
	sent := e.mail.all()
	if len(sent) != 1 || sent[0].To != "unv@x.example" || !strings.Contains(sent[0].Body, "/verify?token=") {
		t.Fatalf("mail = %+v", sent)
	}
	for i := 4; i < 10; i++ {
		if resp, _ := e.post(t, c, "/verify/resend", url.Values{"email": {"nobody@x.example"}}); resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i+1, resp.StatusCode)
		}
	}
	if resp, body := e.post(t, c, "/verify/resend", url.Values{"email": {"nobody@x.example"}}); resp.StatusCode != 429 || !strings.Contains(body, msgTooMany) {
		t.Fatalf("11th: %d %s", resp.StatusCode, body)
	}
}

func TestUnverifiedLoginWithoutMail(t *testing.T) {
	e := newEnv(t)
	e.addUnverified(t, "unv@x.example")
	c := e.client(t)
	resp, body := e.post(t, c, "/login", url.Values{"email": {"unv@x.example"}, "password": {testSecret}})
	if resp.StatusCode != 403 || !strings.Contains(body, msgUnverified) || strings.Contains(body, "/verify/resend") {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.post(t, c, "/verify/resend", url.Values{"email": {"unv@x.example"}}); resp.StatusCode != 404 {
		t.Fatalf("resend: %d", resp.StatusCode)
	}
	if _, body := e.get(t, c, "/login"); strings.Contains(body, "/forgot") {
		t.Fatal("forgot link without mail")
	}
}

func TestOpenSignupForm(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c := e.client(t)
	resp, body := e.get(t, c, "/signup")
	if resp.StatusCode != 200 || !strings.Contains(body, `name="workspace"`) {
		t.Fatalf("open form: %d %s", resp.StatusCode, body)
	}
	if _, body := e.get(t, c, "/login"); !strings.Contains(body, `href="/signup"`) || !strings.Contains(body, `href="/forgot"`) {
		t.Fatalf("login links: %s", body)
	}
}

func TestOpenWithoutMailerRefused(t *testing.T) {
	e := newEnvMail(t, openCfg)
	if _, err := New(Options{Config: e.cfg, Store: e.st, Monitor: e.mon}); err == nil {
		t.Fatal("New accepted signup open without a mailer")
	}
}

func TestUnverifiedStatusPage404(t *testing.T) {
	e := newEnvMail(t, openCfg)
	link := e.openSignedUp(t, "127.0.0.2", "new@x.example")
	c := e.client(t)
	if resp, _ := e.get(t, c, "/s/acme-team"); resp.StatusCode != 404 {
		t.Fatalf("unverified: %d", resp.StatusCode)
	}
	e.post(t, c, "/verify", url.Values{"token": {linkToken(link)}})
	if resp, _ := e.get(t, c, "/s/acme-team"); resp.StatusCode != 200 {
		t.Fatalf("verified: %d", resp.StatusCode)
	}
}

func TestResendDroppedKeepsOldLink(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.openSignedUp(t, "127.0.0.2", "new@x.example")
	c := e.client(t)
	e.post(t, c, "/verify/resend", url.Values{"email": {"new@x.example"}})
	sent := e.mail.all()
	if len(sent) != 2 {
		t.Fatalf("mail = %+v", sent)
	}
	link := linkFrom(sent[1].Body)
	e.mail.mu.Lock()
	e.mail.drop = true
	e.mail.mu.Unlock()
	if resp, body := e.post(t, c, "/verify/resend", url.Values{"email": {"new@x.example"}}); resp.StatusCode != 200 || !strings.Contains(body, msgResendSent) {
		t.Fatalf("dropped resend: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.post(t, c, "/verify", url.Values{"token": {linkToken(link)}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("old link invalidated by a dropped resend: %d %s", resp.StatusCode, body)
	}
}

func TestUnverifiedWrongPassword(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.addUnverified(t, "unv@x.example")
	resp, body := e.post(t, e.client(t), "/login", url.Values{"email": {"unv@x.example"}, "password": {"wrong password!"}})
	if resp.StatusCode != 401 || !strings.Contains(body, msgBadLogin) || strings.Contains(body, msgUnverified) {
		t.Fatalf("wrong password: %d %s", resp.StatusCode, body)
	}
}

func TestVerifyBadTokenOffersResend(t *testing.T) {
	e := newEnvMail(t, openCfg)
	resp, body := e.get(t, e.client(t), "/verify?token=bogus")
	if resp.StatusCode != 404 || !strings.Contains(body, msgBadLink) || !strings.Contains(body, `action="/verify/resend"`) {
		t.Fatalf("bad token: %d %s", resp.StatusCode, body)
	}
}

// TestEnumerationTiming checks that /forgot, /verify/resend and open sign-up
// take about as long for an existing address as for an unknown one. It uses
// the real hasher, so a missing "always hash" would show as a gap.
func TestEnumerationTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("uses the real password hasher")
	}
	e := buildEnv(t, password.Default(), openCfg, true)
	const pending = "pending@example.com"
	e.addUnverified(t, pending)

	ipN := 0
	nextIP := func() string {
		ipN++
		return fmt.Sprintf("127.7.%d.%d", ipN/250, ipN%250+1)
	}
	post := func(path string, form url.Values) (int, string) {
		resp, body := e.post(t, e.clientFrom(t, nextIP()), path, form)
		return resp.StatusCode, body
	}
	type scenario struct {
		name string
		do   func(existing bool, i int) (int, string)
	}
	scenarios := []scenario{
		{"forgot", func(existing bool, i int) (int, string) {
			em := fmt.Sprintf("nobody%d@example.com", i)
			if existing {
				em = opEmail
			}
			return post("/forgot", url.Values{"email": {em}})
		}},
		{"resend", func(existing bool, i int) (int, string) {
			em := fmt.Sprintf("nobody%d@example.com", i)
			if existing {
				em = pending
			}
			return post("/verify/resend", url.Values{"email": {em}})
		}},
		{"signup", func(existing bool, i int) (int, string) {
			em := fmt.Sprintf("fresh%d@example.com", i)
			if existing {
				em = opEmail
			}
			return post("/signup", signupForm("", em))
		}},
	}
	med := func(ds []time.Duration) time.Duration {
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[len(ds)/2]
	}
	const n = 30
	for _, sc := range scenarios {
		var known, unknown []time.Duration
		for i := 0; i < n+1; i++ {
			type result struct {
				status int
				body   string
				d      time.Duration
			}
			try := func(existing bool) result {
				start := time.Now()
				st, body := sc.do(existing, i)
				return result{st, body, time.Since(start)}
			}
			e.advance(25 * time.Hour)
			var k, u result
			if i%2 == 0 { // alternate which path goes first
				k = try(true)
				u = try(false)
			} else {
				u = try(false)
				k = try(true)
			}
			if k.status != u.status || k.body != u.body {
				t.Fatalf("%s round %d: responses differ: %d vs %d, bodies equal=%v", sc.name, i, k.status, u.status, k.body == u.body)
			}
			if i > 0 { // round 0 is warm-up
				known, unknown = append(known, k.d), append(unknown, u.d)
			}
		}
		mk, mu := med(known), med(unknown)
		t.Logf("%s: median existing=%v unknown=%v", sc.name, mk, mu)
		if diff := mk - mu; diff >= 50*time.Millisecond || -diff >= 50*time.Millisecond {
			t.Fatalf("%s: timing differs by %v (existing %v, unknown %v)", sc.name, diff, mk, mu)
		}
	}
}
