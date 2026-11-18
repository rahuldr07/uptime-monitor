package web

import (
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/mail"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

func TestSettingsRedirectsToProfile(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.get(t, e.op, "/settings")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings/profile" {
		t.Fatalf("GET /settings: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSettingsSections(t *testing.T) {
	e := newEnv(t)
	// The operator cannot delete their account, so Danger zone is for tenants.
	tenant, _ := e.account(t, "t@x.example")
	for _, c := range []struct {
		path, label string
		want        []string
	}{
		{"/settings/profile", "Profile", []string{"Time zone"}},
		{"/settings/security", "Security", []string{"Sign-in methods", "Sessions"}},
		{"/settings/danger", "Danger zone", []string{"Delete account"}},
	} {
		resp, body := e.get(t, tenant, c.path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", c.path, resp.StatusCode)
		}
		for _, w := range c.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", c.path, w)
			}
		}
		if !strings.Contains(body, `<h2 class="sec">`+c.label+`</h2>`) || !strings.Contains(body, "<title>"+c.label+" · Settings · Uptime</title>") {
			t.Errorf("%s: h2 or title does not match the sub-nav label %q", c.path, c.label)
		}
		if n := strings.Count(body, `class="active" aria-current="page"`); n != 1 {
			t.Errorf("%s: %d active sub-nav links", c.path, n)
		}
		if !strings.Contains(body, `class="active" aria-current="page" href="`+c.path+`">`+c.label+`</a>`) {
			t.Errorf("%s: sub-nav link for %s is not the active one", c.path, c.label)
		}
	}
}

func TestSettingsDangerOperatorRedirect(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.get(t, e.op, "/settings/danger")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/settings/profile" {
		t.Fatalf("operator: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSecurityCardOrder(t *testing.T) {
	e := newEnv(t)
	c, _ := e.account(t, "t@x.example")
	_, body := e.get(t, c, "/settings/security")
	last := -1
	for _, h := range []string{"<h3>Password</h3>", "<h3>Sign-in methods</h3>", "<h3>Sessions</h3>"} {
		i := strings.Index(body, h)
		if i <= last {
			t.Fatalf("%s missing or out of order", h)
		}
		last = i
	}
}

func TestAccountRedirects(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.get(t, e.op, "/account")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/settings/security" {
		t.Fatalf("GET /account: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestOldAccountPostAliasesRemoved(t *testing.T) {
	e := newEnv(t)
	c, _ := e.account(t, "t@x.example")
	for _, p := range []string{"/account/timezone", "/account/tz-detected", "/account/password", "/account/signout-all", "/account/delete",
		"/account/identities/google/connect", "/account/identities/google/delete"} {
		if resp, _ := e.post(t, c, p, url.Values{"current": {testSecret}}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: %d, want 404", p, resp.StatusCode)
		}
	}
	// The new paths answer; Delete with a wrong password renders the Danger zone page.
	resp, body := e.post(t, c, "/settings/danger/delete", url.Values{"current": {"wrong wrong wrong"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgBadCurrent) || !strings.Contains(body, "Delete account") {
		t.Errorf("/settings/danger/delete: %d", resp.StatusCode)
	}
	// Still signed in: nothing above touched the account. The GET redirects stay.
	if resp, _ := e.get(t, c, "/account"); resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/settings/security" {
		t.Errorf("GET /account: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestSidebarSettingsLink(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, e.op, "/")
	if !strings.Contains(body, `href="/settings/profile"`) || !strings.Contains(body, `href="#i-settings"`) {
		t.Error("sidebar has no Settings gear link")
	}
	if strings.Contains(body, `href="/account`) {
		t.Error("layout still links to /account")
	}
	if strings.Contains(body, `href="/settings/profile" title="`+opEmail) || strings.Contains(body, `>`+opEmail+`</a>`) {
		t.Error("the sidebar email is still a link")
	}
	if !strings.Contains(body, opEmail) {
		t.Error("the sidebar email is gone")
	}
	if !strings.Contains(body, `<a href="/settings/profile#timezone">Times in `) {
		t.Error("footer does not link to the time zone setting")
	}
}

func TestMailLinksPointToSettings(t *testing.T) {
	_, b := signInMethodMail("https://up.example", "Google", true, time.Now(), time.UTC)
	if !strings.Contains(b, "https://up.example/settings/security") || strings.Contains(b, "/account") {
		t.Errorf("signInMethodMail: %s", b)
	}
	if _, b = passwordChangedMail("https://up.example"); strings.Contains(b, "/account") {
		t.Errorf("passwordChangedMail: %s", b)
	}
}

func TestSettingsNotifications(t *testing.T) {
	e := newEnv(t)
	resp, body := e.get(t, e.op, "/settings/notifications")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, w := range []string{"Telegram", "Slack", ">Email</label>", `action="/settings/notifications"`, `class="active" aria-current="page" href="/settings/notifications">Notifications</a>`} {
		if !strings.Contains(body, w) {
			t.Errorf("page lacks %q", w)
		}
	}
	// The v1.4.0 fallback copy shows when the server has mail and no SMTP host is set.
	f, _, _ := fallbackEnv(t)
	if _, body := f.get(t, f.op, "/settings/notifications"); !strings.Contains(body, "Alerts are emailed from this server to "+opEmail) {
		t.Error("fallback copy missing")
	}
}

func TestAlertsRedirect(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.get(t, e.op, "/alerts")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/settings/notifications" {
		t.Fatalf("GET /alerts: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAlertsPostAliasRemoved(t *testing.T) {
	e := newEnv(t)
	// The path still exists for GET, so the mux answers 405 rather than 404; either way nothing is saved.
	for _, p := range []string{"/alerts", "/alerts/test"} {
		resp, _ := e.post(t, e.op, p, url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}})
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", p, resp.StatusCode)
		}
	}
	if n, err := e.opWS.Notify(t.Context()); err == nil && n.Telegram != nil {
		t.Fatalf("a removed alias saved: %+v", n)
	}
	resp, _ := e.post(t, e.op, "/settings/notifications", url.Values{"tg_token": {"tok"}, "tg_chat": {"2"}, "tg_enabled": {"1"}})
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/settings/notifications" || m != "Alert channels saved." {
		t.Fatalf("POST /settings/notifications: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, body := e.post(t, e.op, "/settings/notifications/test", url.Values{"channel": {"nope"}}); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "failed") {
		t.Errorf("test send: %d", resp.StatusCode)
	}
}

func TestSettingsNotificationsTenant(t *testing.T) {
	e := newEnv(t)
	e.post(t, e.op, "/settings/notifications", url.Values{"tg_token": {"optoken"}, "tg_chat": {"OPCHAT"}, "tg_enabled": {"1"}})
	c, _ := e.account(t, "t@x.example")
	resp, body := e.get(t, c, "/settings/notifications")
	if resp.StatusCode != 200 {
		t.Fatalf("tenant: %d", resp.StatusCode)
	}
	if strings.Contains(body, "OPCHAT") || strings.Contains(body, "Saved · leave blank to keep") {
		t.Error("tenant sees the operator's channels")
	}
	for _, href := range []string{"/settings/profile", "/settings/security", "/settings/notifications", "/settings/danger"} {
		if !strings.Contains(body, `href="`+href+`"`) {
			t.Errorf("sub-nav lacks %s", href)
		}
	}
	// The operator cannot delete their account, so the sub-nav leaves that page out.
	_, op := e.get(t, e.op, "/settings/notifications")
	if strings.Contains(op, `href="/settings/danger"`) {
		t.Error("operator sub-nav links to Danger zone")
	}
}

func TestMainNavHasNoAlertChannels(t *testing.T) {
	e := newEnv(t)
	_, body := e.get(t, e.op, "/")
	if strings.Contains(body, `href="/alerts"`) || strings.Contains(body, "Alert channels</span>") {
		t.Error("main nav still has Alert channels")
	}
	if !strings.Contains(body, `href="/settings/notifications"`) {
		t.Error("overview onboarding no longer links to the channels")
	}
}

const setPwLink = "/settings/security/password-link"

func TestSetPasswordLinkSends(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, u := o.socialClient(t, "sam@example.com", "google", "12345")
	before := len(o.mail.all())
	resp, _ := o.post(t, c, setPwLink, nil)
	if loc := resp.Header.Get("Location"); strings.Contains(loc, "sam") || strings.Contains(loc, "example.com") {
		t.Errorf("the address is in the URL: %s", loc)
	}
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/settings/security" || m != "Link sent." {
		t.Fatalf("POST: %d %s %q", resp.StatusCode, p, m)
	}
	if !strings.Contains(resp.Header.Get("Location"), "sent=1") {
		t.Errorf("Location lacks sent=1: %s", resp.Header.Get("Location"))
	}
	sent := o.mail.all()[before:]
	if len(sent) != 1 || sent[0].To != "sam@example.com" || sent[0].Subject != "Set a password for your Uptime account" || !strings.Contains(sent[0].Body, "/reset?token=") {
		t.Fatalf("mail = %+v", sent)
	}
	tok, err := o.st.PeekToken(t.Context(), store.TokenReset, linkToken(linkFrom(sent[0].Body)), o.now())
	if err != nil || tok.UserID != u.ID {
		t.Fatalf("token: %+v %v", tok, err)
	}
	// ?sent=1 shows the persistent state with a secondary Send again; without it the card offers the primary button.
	_, body := o.get(t, c, "/settings/security?sent=1")
	if !strings.Contains(body, "Check sam@example.com for a link to set your password. It works for 1 hour.") || !strings.Contains(body, "Send again") || strings.Contains(body, "Email me a link") {
		t.Errorf("after sent: %s", body)
	}
	if n := strings.Count(body, "works for 1 hour"); n != 1 {
		t.Errorf("the sent state says how long the link works %d times, want once", n)
	}
	_, body = o.get(t, c, "/settings/security")
	if !strings.Contains(body, "Email me a link") || strings.Contains(body, "Send again") || strings.Contains(body, "Check sam@example.com") {
		t.Errorf("before sent: %s", body)
	}
}

func TestSetPasswordLinkDroppedIsNotSuccess(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	before := len(o.mail.all())
	o.mail.mu.Lock()
	o.mail.drop = true
	o.mail.mu.Unlock()
	// Dropped sends give the user's slot back, so more than 3 attempts get the same honest answer.
	for i := 0; i < 5; i++ {
		resp, _ := o.post(t, c, setPwLink, nil)
		p, m := flashOf(t, resp)
		if resp.StatusCode != http.StatusSeeOther || m != msgPwLinkTooMany || strings.Contains(resp.Header.Get("Location"), "sent=1") || p != "/settings/security" {
			t.Fatalf("dropped %d: %d %s %q", i, resp.StatusCode, resp.Header.Get("Location"), m)
		}
	}
	if len(o.mail.all()) != before {
		t.Error("mail sent while dropping")
	}
	o.mail.mu.Lock()
	o.mail.drop = false
	o.mail.mu.Unlock()
	if _, m := flashOf(t, mustPost(t, o, c, setPwLink)); m != msgPwLinkSent {
		t.Errorf("after the drop: %q", m)
	}
}

func TestSetPasswordLinkRecipientBudgetUsedUp(t *testing.T) {
	// The real queue: /forgot three times on the address uses up its hourly budget.
	o := newOEnv(t, `{}`, "google")
	q := mail.New(mail.Options{Sender: &sinkSender{}, PerHour: 1000})
	o.srv.mail = q
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	for i := 0; i < 3; i++ {
		o.forgot(t, "127.0.0.9", "sam@example.com")
	}
	resp, _ := o.post(t, c, setPwLink, nil)
	p, m := flashOf(t, resp)
	if m != msgPwLinkTooMany || strings.Contains(resp.Header.Get("Location"), "sent=1") || p != "/settings/security" {
		t.Fatalf("after the recipient budget was used: %d %s %q", resp.StatusCode, resp.Header.Get("Location"), m)
	}
}

func TestSetPasswordLinkAddressLimitFirst(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	req := httptest.NewRequest("POST", "/", nil)
	req.RemoteAddr = "127.0.0.1:1"
	key := clientKey(req, o.cfg.ProxyHops())
	for {
		if ok, _ := o.srv.mailByAddr.Take(key); !ok {
			break
		}
	}
	before := len(o.mail.all())
	if _, m := flashOf(t, mustPost(t, o, c, setPwLink)); m != msgPwLinkTooMany {
		t.Fatalf("address budget: %q", m)
	}
	if len(o.mail.all()) != before {
		t.Error("sent despite the address budget")
	}
	// The refusal did not use the user's own allowance.
	uid := strconv.FormatInt(o.userID(t, "sam@example.com"), 10)
	for i := 0; i < pwLinkPerHour; i++ {
		if ok, _ := o.srv.pwLinkByUser.Take(uid); !ok {
			t.Fatalf("slot %d already used", i)
		}
	}
}

func TestSetPasswordLinkSendFailure(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	o.mail.mu.Lock()
	o.mail.fail = errors.New("smtp down")
	o.mail.mu.Unlock()
	for i := 0; i < 4; i++ { // the slot is refunded each time
		resp, body := o.post(t, c, setPwLink, nil)
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, html.EscapeString(msgPwLinkFailed)) || strings.Contains(body, "Check sam@example.com") {
			t.Fatalf("failure %d: %d", i, resp.StatusCode)
		}
	}
	if strings.Contains(o.logs.String(), "/reset?token") || strings.Contains(o.logs.String(), "token=") {
		t.Error("a link was logged")
	}
	if !strings.Contains(o.logs.String(), "cannot send link") {
		t.Error("failure not logged")
	}
}

func TestSetPasswordLinkLimit(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	before := len(o.mail.all())
	for i := 0; i < 3; i++ {
		if resp, _ := o.post(t, c, setPwLink, nil); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := o.post(t, c, setPwLink, nil)
	if _, m := flashOf(t, resp); m != msgPwLinkTooMany {
		t.Errorf("4th POST: %d %q", resp.StatusCode, m)
	}
	if n := len(o.mail.all()) - before; n != 3 {
		t.Errorf("%d messages sent, want 3", n)
	}
	// An hour later the budget is back.
	o.advance(time.Hour + time.Second)
	if _, m := flashOf(t, mustPost(t, o, c, setPwLink)); strings.Contains(m, "recently") {
		t.Errorf("after an hour: %q", m)
	}
}

func TestSetPasswordLinkOnlyWithoutPassword(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.account(t, "pw@example.com")
	before := len(o.mail.all())
	resp, _ := o.post(t, c, setPwLink, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status %d, want 409", resp.StatusCode)
	}
	if n := len(o.mail.all()) - before; n != 0 {
		t.Errorf("%d messages sent", n)
	}
}

func TestSetPasswordCardWithMail(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	_, body := o.get(t, c, "/settings/security")
	if !strings.Contains(body, `action="/settings/security/password-link"`) || !strings.Contains(body, "Email me a link</button>") ||
		strings.Contains(body, `name="new"`) {
		t.Errorf("with mail: %s", body)
	}
	// The in-place form is gone, so its POST no longer sets a first password either.
	u := o.userID(t, "sam@example.com")
	for _, p := range []string{"/settings/security/password"} {
		resp, body := o.post(t, c, p, newPW)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(body, msgPwLinkUseEmail) {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
		if has, _ := o.st.HasPassword(t.Context(), u); has {
			t.Fatalf("%s set a first password in place on an install with mail", p)
		}
	}
}

func TestSetPasswordNoMailFallback(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.srv.mail = nil
	c, u := o.socialClient(t, "sam@example.com", "google", "12345")
	_, body := o.get(t, c, "/settings/security")
	if strings.Contains(body, "password-link") || !strings.Contains(body, `name="new"`) || !strings.Contains(body, "Set a password</button>") {
		t.Errorf("without mail: %s", body)
	}
	if resp, _ := o.post(t, c, setPwLink, nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("password-link without mail: %d", resp.StatusCode)
	}
	// Outside the recent sign-in window the form is replaced by the re-sign-in rule.
	o.advance(recentAuthWindow + time.Second)
	if resp, _ := o.post(t, c, "/settings/security/password", newPW); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("outside window: %d", resp.StatusCode)
	}
	if r, _ := o.run(t, c, "google", "intent=reauth&next=%2Fsettings%2Fsecurity"); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("reauth: %d", r.StatusCode)
	}
	resp, _ := o.post(t, c, "/settings/security/password", newPW)
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/settings/security" || m != msgPasswordChanged {
		t.Fatalf("set: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if has, _ := o.st.HasPassword(t.Context(), u.ID); !has {
		t.Fatal("password not set")
	}
}

func TestResetWordingFollowsCurrentState(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, u := o.socialClient(t, "sam@example.com", "google", "12345")
	o.post(t, c, setPwLink, nil)
	sent := o.mail.all()
	link := linkFrom(sent[len(sent)-1].Body)
	_, body := o.get(t, o.client(t), linkPath(link))
	if !strings.Contains(body, "Choose a password") || strings.Contains(body, "Reset password") || !strings.Contains(body, "You can still sign in with Google.") || strings.Contains(body, "sam@example.com") {
		t.Errorf("no password yet: %s", body)
	}
	// A failed submit keeps the wording.
	if resp, body := o.post(t, o.client(t), "/reset", url.Values{"token": {linkToken(link)}, "password": {"short"}, "confirm": {"short"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Choose a password") {
		t.Errorf("failed submit: %d %s", resp.StatusCode, body)
	}
	if code, b := o.resetWith(t, link, newSecretPW); code != http.StatusSeeOther {
		t.Fatalf("set: %d %s", code, b)
	}
	if has, _ := o.st.HasPassword(t.Context(), u.ID); !has {
		t.Fatal("password not set")
	}
	// Any later link (Forgot password) reads as a reset, and works.
	raw, err := o.st.CreateToken(t.Context(), store.TokenReset, u.ID, resetTTL, o.now())
	if err != nil {
		t.Fatal(err)
	}
	_, body = o.get(t, o.client(t), "/reset?token="+raw)
	if !strings.Contains(body, "Reset password") || strings.Contains(body, "Choose a password") {
		t.Errorf("has password: %s", body)
	}
	if code, b := o.resetWith(t, "x?token="+raw, testSecret); code != http.StatusSeeOther {
		t.Fatalf("reset: %d %s", code, b)
	}
	if r, _ := o.post(t, o.client(t), "/login", url.Values{"email": {"sam@example.com"}, "password": {testSecret}}); r.StatusCode != http.StatusSeeOther {
		t.Errorf("sign-in with the new password: %d", r.StatusCode)
	}
}

func TestSetPasswordMail(t *testing.T) {
	subject, body := setPasswordMail("https://up.example/reset?token=abc")
	if subject != "Set a password for your Uptime account" || !strings.Contains(body, "https://up.example/reset?token=abc") || !strings.Contains(body, "1 hour") {
		t.Errorf("%q %q", subject, body)
	}
}

func mustPost(t *testing.T, o *oenv, c *http.Client, path string) *http.Response {
	t.Helper()
	resp, _ := o.post(t, c, path, nil)
	return resp
}

// The link is the same one /forgot sends, so D-126 holds: the same user's session is kept and signed in, another user's is left alone.
func TestSetPasswordLinkWithSessions(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c, _ := o.socialClient(t, "sam@example.com", "google", "12345")
	other, _ := o.account(t, "other@example.com")
	link := func() string {
		o.post(t, c, setPwLink, nil)
		sent := o.mail.all()
		return linkFrom(sent[len(sent)-1].Body)
	}
	// Another user's session: the password is set, nobody is signed in as Sam.
	l := link()
	resp, body := o.post(t, other, "/reset", url.Values{"token": {linkToken(l)}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, msgPasswordChangedSignIn) {
		t.Fatalf("other user's session: %d %s", resp.StatusCode, body)
	}
	if !signedIn(t, o, other) {
		t.Error("the other user's session was replaced")
	}
	if signedIn(t, o, c) {
		t.Error("Sam's old session survived a password set")
	}
	// Sam's own session (a fresh account without a password again): kept, and the reset signs in.
	c2, _ := o.socialClient(t, "sam2@example.com", "google", "777")
	o.post(t, c2, setPwLink, nil)
	sent := o.mail.all()
	resp, body = o.post(t, c2, "/reset", url.Values{"token": {linkToken(linkFrom(sent[len(sent)-1].Body))}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("same user's session: %d %s", resp.StatusCode, body)
	}
	if !signedIn(t, o, c2) {
		t.Error("Sam is not signed in after choosing a password")
	}
}
