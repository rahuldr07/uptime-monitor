package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func (e *env) forgot(t *testing.T, ip, email string) (int, string) {
	t.Helper()
	resp, body := e.forgotResp(t, ip, email)
	return resp.StatusCode, body
}

func (e *env) forgotResp(t *testing.T, ip, email string) (*http.Response, string) {
	t.Helper()
	return e.post(t, e.clientFrom(t, ip), "/forgot", url.Values{"email": {email}})
}

func sessionValue(t *testing.T, e *env, c *http.Client) string {
	t.Helper()
	u, _ := url.Parse(e.ts.URL)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == "uptime_session" {
			return ck.Value
		}
	}
	return ""
}

func (e *env) resetWith(t *testing.T, link, pw string) (int, string) {
	t.Helper()
	resp, body := e.post(t, e.client(t), "/reset", url.Values{"token": {linkToken(link)}, "password": {pw}, "confirm": {pw}})
	return resp.StatusCode, body
}

func TestForgotSendsResetLink(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "a@x.example")
	if code, body := e.forgot(t, "127.0.0.2", "a@x.example"); code != 200 || !strings.Contains(body, msgForgotSent) {
		t.Fatalf("forgot: %d %s", code, body)
	}
	sent := e.mail.all()
	if len(sent) != 1 || sent[0].To != "a@x.example" || !strings.Contains(sent[0].Body, "/reset?token=") {
		t.Fatalf("mail = %+v", sent)
	}
	link := linkFrom(sent[0].Body)
	if resp, body := e.get(t, e.client(t), linkPath(link)); resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) {
		t.Fatalf("GET link: %d %s", resp.StatusCode, body)
	}
	if code, body := e.resetWith(t, link, newSecretPW); code != 303 {
		t.Fatalf("reset: %d %s", code, body)
	}
}

func TestForgotIdentical(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ver@x.example")
	e.addUnverified(t, "unv@x.example")
	e.account(t, "dis@x.example")
	e.disable(t, "dis@x.example")
	var first string
	for i, em := range []string{"nobody@x.example", "ver@x.example", "unv@x.example", "dis@x.example"} {
		resp, body := e.forgotResp(t, "127.0.0.2", em)
		code := resp.StatusCode
		if len(resp.Header.Values("Set-Cookie")) != 0 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: headers %v", em, resp.Header)
		}
		if code != 200 || !strings.Contains(body, msgForgotSent) {
			t.Fatalf("%s: %d %s", em, code, body)
		}
		if i == 0 {
			first = body
		} else if body != first {
			t.Fatalf("%s: body differs", em)
		}
	}
	sent := e.mail.all()
	if len(sent) != 2 || sent[0].To != "ver@x.example" || sent[1].To != "unv@x.example" {
		t.Fatalf("mail = %+v", sent)
	}
	if code, body := e.resetWith(t, linkFrom(sent[1].Body), newSecretPW); code != 303 {
		t.Fatalf("reset: %d %s", code, body)
	}
	if !e.verified(t, "unv@x.example") {
		t.Fatal("reset did not verify")
	}
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {"unv@x.example"}, "password": {newSecretPW}}); resp.StatusCode != 303 {
		t.Fatalf("sign-in: %d", resp.StatusCode)
	}
}

func TestForgotRateLimit(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "a@x.example")
	for i := 0; i < 6; i++ {
		em := "nobody@x.example"
		if i%2 == 0 {
			em = "a@x.example"
		}
		if code, _ := e.forgot(t, "127.0.0.2", em); code != 200 {
			t.Fatalf("forgot %d: %d", i, code)
		}
	}
	c := e.clientFrom(t, "127.0.0.2")
	for i := 0; i < 4; i++ {
		em := "nobody@x.example"
		if i%2 == 0 {
			e.addUnverified(t, "u"+string(rune('a'+i))+"@x.example")
			em = "u" + string(rune('a'+i)) + "@x.example"
		}
		if resp, _ := e.post(t, c, "/verify/resend", url.Values{"email": {em}}); resp.StatusCode != 200 {
			t.Fatalf("resend %d: %d", i, resp.StatusCode)
		}
	}
	if code, body := e.forgot(t, "127.0.0.2", "nobody@x.example"); code != 429 || !strings.Contains(body, msgTooMany) {
		t.Fatalf("11th: %d %s", code, body)
	}
}

func TestForgotDroppedKeepsOldLink(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "a@x.example")
	e.forgot(t, "127.0.0.2", "a@x.example")
	link := linkFrom(e.mail.all()[0].Body)
	e.mail.drop = true
	if code, body := e.forgot(t, "127.0.0.2", "a@x.example"); code != 200 || !strings.Contains(body, msgForgotSent) {
		t.Fatalf("dropped: %d %s", code, body)
	}
	if code, body := e.resetWith(t, link, newSecretPW); code != 303 {
		t.Fatalf("old link: %d %s", code, body)
	}
}

func TestForgotWithoutMail404(t *testing.T) {
	e := newEnv(t)
	if resp, _ := e.get(t, e.client(t), "/forgot"); resp.StatusCode != 404 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	if code, _ := e.forgot(t, "127.0.0.2", "a@x.example"); code != 404 {
		t.Fatalf("POST: %d", code)
	}
}

func TestResetOtherUsersSession(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "a@x.example")
	b, _ := e.account(t, "b@x.example")
	e.forgot(t, "127.0.0.2", "a@x.example")
	link := linkFrom(e.mail.all()[0].Body)
	resp, body := e.post(t, b, "/reset", url.Values{"token": {linkToken(link)}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != 200 || !strings.Contains(body, msgPasswordChangedSignIn) || len(resp.Header.Values("Set-Cookie")) != 0 {
		t.Fatalf("reset as B: %d %v %s", resp.StatusCode, resp.Header.Values("Set-Cookie"), body)
	}
	if e.status(t, b) != 200 {
		t.Fatal("B's session was disturbed")
	}
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {"a@x.example"}, "password": {newSecretPW}}); resp.StatusCode != 303 {
		t.Fatalf("A new password: %d", resp.StatusCode)
	}
	if n := len(e.mail.all()); n != 2 {
		t.Fatalf("want reset link + change notice, got %d mails", n)
	}
}

func TestResetSignedInSameUser(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "a@x.example")
	e.forgot(t, "127.0.0.2", "a@x.example")
	link := linkFrom(e.mail.all()[0].Body)
	old := sessionValue(t, e, c)
	if old == "" {
		t.Fatal("no session cookie before reset")
	}
	resp, body := e.post(t, c, "/reset", url.Values{"token": {linkToken(link)}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" || cookieNamed(resp, "uptime_session") == nil {
		t.Fatalf("reset: %d %s %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if e.status(t, c) != 200 {
		t.Fatal("new session does not work")
	}
	if now := sessionValue(t, e, c); now == old {
		t.Fatal("session cookie was not replaced")
	}
	stale := e.client(t)
	u, _ := url.Parse(e.ts.URL)
	stale.Jar.SetCookies(u, []*http.Cookie{{Name: "uptime_session", Value: old, Path: "/"}})
	if e.status(t, stale) != 401 {
		t.Fatal("old session still signs in")
	}
}

func TestResetStaleCookieSignsIn(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "a@x.example")
	e.forgot(t, "127.0.0.2", "a@x.example")
	link := linkFrom(e.mail.all()[0].Body)
	c := e.client(t)
	u, _ := url.Parse(e.ts.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: "uptime_session", Value: "stale-garbage", Path: "/"}})
	resp, body := e.post(t, c, "/reset", url.Values{"token": {linkToken(link)}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != 303 || cookieNamed(resp, "uptime_session") == nil {
		t.Fatalf("reset: %d %s", resp.StatusCode, body)
	}
	if e.status(t, c) != 200 {
		t.Fatal("fresh session does not work")
	}
}

func TestResetPageShownToSignedIn(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "a@x.example")
	e.forgot(t, "127.0.0.2", "a@x.example")
	link := linkFrom(e.mail.all()[0].Body)
	resp, body := e.get(t, c, linkPath(link))
	if resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) {
		t.Fatalf("GET /reset signed in: %d %s", resp.StatusCode, body)
	}
}

func TestForgotNoStore(t *testing.T) {
	e := newEnvMail(t, openCfg)
	if resp, _ := e.get(t, e.client(t), "/forgot"); resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if resp, _ := e.forgotResp(t, "127.0.0.2", "a@x.example"); resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("POST Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func TestPasswordChangeNotice(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "a@x.example")
	if resp, _ := e.post(t, c, "/account/password", url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {newSecretPW}}); resp.StatusCode != 303 {
		t.Fatalf("change: %d", resp.StatusCode)
	}
	sent := e.mail.all()
	if len(sent) != 1 || sent[0].To != "a@x.example" || sent[0].Subject != "Your password was changed" {
		t.Fatalf("mail after change = %+v", sent)
	}
	e.forgot(t, "127.0.0.2", "a@x.example")
	link := linkFrom(e.mail.all()[1].Body)
	e.resetWith(t, link, testSecret)
	sent = e.mail.all()
	if len(sent) != 3 || sent[2].Subject != "Your password was changed" {
		t.Fatalf("mail after reset = %+v", sent)
	}
}
