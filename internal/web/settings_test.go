package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

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
		if n := strings.Count(body, `class="active" aria-current="page"`); n != 1 {
			t.Errorf("%s: %d active sub-nav links", c.path, n)
		}
		if !strings.Contains(body, `class="active" aria-current="page" href="`+c.path+`">`+c.label+`</a>`) {
			t.Errorf("%s: sub-nav link for %s is not the active one", c.path, c.label)
		}
	}
}

func TestAccountRedirects(t *testing.T) {
	e := newEnv(t)
	resp, _ := e.get(t, e.op, "/account")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/settings/security" {
		t.Fatalf("GET /account: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestOldAccountPostAliases(t *testing.T) {
	e := newEnv(t)
	c, _ := e.account(t, "t@x.example")
	cur, next := testSecret, newSecretPW // each password change swaps them
	for _, k := range []struct {
		old, new string
		form     func(cur, next string) url.Values
		page     string
	}{
		{"/account/timezone", "/settings/profile/timezone", func(_, _ string) url.Values { return url.Values{"time_zone": {"UTC"}} }, "/settings/profile"},
		{"/account/password", "/settings/security/password", func(cur, next string) url.Values {
			return url.Values{"current": {cur}, "new": {next}, "confirm": {next}}
		}, "/settings/security"},
	} {
		var got [2]*http.Response
		for i, p := range []string{k.old, k.new} {
			got[i], _ = e.post(t, c, p, k.form(cur, next))
			if got[i].StatusCode == http.StatusSeeOther && strings.Contains(p, "password") {
				cur, next = next, cur
			}
		}
		po, mo := flashOf(t, got[0])
		pn, mn := flashOf(t, got[1])
		if got[0].StatusCode != http.StatusSeeOther || po != k.page || mo == "" {
			t.Errorf("%s: %d %s", k.old, got[0].StatusCode, got[0].Header.Get("Location"))
		}
		if got[1].StatusCode != got[0].StatusCode || pn != po || mn != mo {
			t.Errorf("%s: %d %s %q, old gave %d %s %q", k.new, got[1].StatusCode, pn, mn, got[0].StatusCode, po, mo)
		}
	}
	// Delete with a wrong password renders the Danger zone page on both paths.
	bad := url.Values{"current": {"wrong wrong wrong"}}
	for _, p := range []string{"/account/delete", "/settings/danger/delete"} {
		resp, body := e.post(t, c, p, bad)
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, msgBadCurrent) || !strings.Contains(body, "Delete account") {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	// Sign out everywhere, last: the session goes.
	for i, p := range []string{"/account/signout-all", "/settings/security/signout-all"} {
		cc, _ := e.account(t, "s"+string(rune('a'+i))+"@x.example")
		resp, _ := e.post(t, cc, p, nil)
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Errorf("%s: %d %s", p, resp.StatusCode, resp.Header.Get("Location"))
		}
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

func TestAlertsPostAlias(t *testing.T) {
	e := newEnv(t)
	form := url.Values{"tg_token": {"tok"}, "tg_chat": {"1"}, "tg_enabled": {"1"}}
	resp, _ := e.post(t, e.op, "/alerts", form)
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/settings/notifications" || m != "Alert channels saved." {
		t.Fatalf("POST /alerts: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	n, err := e.opWS.Notify(t.Context())
	if err != nil || n.Telegram == nil || n.Telegram.ChatID != "1" {
		t.Fatalf("alias did not save: %+v %v", n, err)
	}
	// A save through the new path gives the same answer.
	resp, _ = e.post(t, e.op, "/settings/notifications", url.Values{"tg_token": {"tok"}, "tg_chat": {"2"}, "tg_enabled": {"1"}})
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/settings/notifications" || m != "Alert channels saved." {
		t.Fatalf("POST /settings/notifications: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Test sends through the alias fail the same way as through the new path.
	for _, p := range []string{"/alerts/test", "/settings/notifications/test"} {
		if resp, body := e.post(t, e.op, p, url.Values{"channel": {"nope"}}); resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "failed") {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
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
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/settings/security" || m != "We sent a link to s…@example.com. It works for 1 hour." {
		t.Fatalf("POST: %d %s %q", resp.StatusCode, p, m)
	}
	sent := o.mail.all()[before:]
	if len(sent) != 1 || sent[0].To != "sam@example.com" || sent[0].Subject != "Set a password for your Uptime account" || !strings.Contains(sent[0].Body, "/reset?token=") {
		t.Fatalf("mail = %+v", sent)
	}
	tok, err := o.st.PeekToken(t.Context(), store.TokenReset, linkToken(linkFrom(sent[0].Body)), o.now())
	if err != nil || tok.UserID != u.ID {
		t.Fatalf("token: %+v %v", tok, err)
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
	if !strings.Contains(body, `action="/settings/security/password-link"`) || !strings.Contains(body, "Set a password</button>") ||
		strings.Contains(body, `name="new"`) {
		t.Errorf("with mail: %s", body)
	}
	// The in-place form is gone, so its POST no longer sets a first password either.
	if resp, _ := o.post(t, c, "/settings/security/password", newPW); resp.StatusCode == http.StatusSeeOther {
		t.Error("first password set in place on an install with mail")
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
	if !strings.Contains(body, "Choose a password") || strings.Contains(body, "Reset password") {
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

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"sam@example.com": "s…@example.com",
		"a@x.com":         "…@x.com",
		"ab@x.com":        "a…@x.com",
		"bad":             "your email",
		"":                "your email",
		"@x.com":          "your email",
		"a@":              "your email",
		"é@x.com":         "…@x.com",
		"éa@x.com":        "é…@x.com",
	} {
		if got := maskEmail(in); got != want {
			t.Errorf("maskEmail(%q) = %q, want %q", in, got, want)
		}
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
