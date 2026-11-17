package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
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
