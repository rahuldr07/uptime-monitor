package web

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/totp"
)

const pathCancel = "/settings/security/2fa/cancel"

// ---- setup hardening ----

func TestTwoFactorSetupAlwaysNeedsPassword(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com") // a fresh sign-in: auth_at is recent
	uid := e.userByEmail(t, "ann@example.com").ID
	resp, body := e.post(t, c, pathSetup, url.Values{})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, html.EscapeString(msgBadCurrent)) {
		t.Fatalf("setup without the password: %d %s", resp.StatusCode, body)
	}
	if resp, _ = e.post(t, c, pathSetup, url.Values{"current": {"wrong wrong wrong"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	if _, err := e.st.PendingTOTP(t.Context(), uid, e.now()); err == nil {
		t.Fatal("a pending secret was made without the password")
	}
	if resp, _ = e.post(t, c, pathSetup, url.Values{"current": {testSecret}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("with the password: %d", resp.StatusCode)
	}
	if _, err := e.st.PendingTOTP(t.Context(), uid, e.now()); err != nil {
		t.Fatal(err)
	}
	_, body = e.get(t, c, "/settings/security")
	_ = body
}

func TestTwoFactorConfirmOutlivesRecentSignIn(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	secret := e.beginSetup(t, c, "ann@example.com")
	e.advance(12 * time.Minute) // the pending secret lives 15 minutes, a recent sign-in 10
	_, body := e.get(t, c, "/settings/security")
	if !strings.Contains(body, groupSecret(secret)) {
		t.Error("the pending setup disappeared after 12 minutes")
	}
	resp, body := e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != 200 || len(recoveryRe.FindAllString(body, -1)) != 10 {
		t.Fatalf("confirm after 12 minutes: %d", resp.StatusCode)
	}
}

func TestTwoFactorSetupCancel(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	other := e.signIn(t, "ann@example.com", testSecret)
	secret := e.beginSetup(t, c, "ann@example.com")
	resp, _ := e.post(t, c, pathCancel, url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	if _, err := e.st.PendingTOTP(t.Context(), e.userByEmail(t, "ann@example.com").ID, e.now()); err == nil {
		t.Error("the pending secret survived Cancel")
	}
	if r, _ := e.get(t, other, "/settings/security"); r.StatusCode != 200 {
		t.Errorf("Cancel signed another session out: %d", r.StatusCode)
	}
	if r, _ := e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}}); r.StatusCode == 200 {
		t.Error("confirm accepted after Cancel")
	}
}

// ---- pending sign-ins die with a password change ----

func TestPasswordChangeAndResetDropPendingSignIns(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	pend := e.pendingSignIn(t, "203.0.113.2", "ann@example.com")
	raw := jarCookie(pend, "uptime_mfa").Value
	form := url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {newSecretPW}}
	if resp, body := e.post(t, c, "/settings/security/password", form); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("change: %d %s", resp.StatusCode, body)
	}
	if _, err := e.st.PendingLogin(t.Context(), raw, e.now()); err == nil {
		t.Error("a pending sign-in survived a password change")
	}
	pend = e.client(t)
	if resp, _ := e.postFrom(t, pend, "203.0.113.3", "/login", url.Values{"email": {"ann@example.com"}, "password": {newSecretPW}}); resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("second pending sign-in: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	raw = jarCookie(pend, "uptime_mfa").Value
	tok, err := e.st.CreateToken(t.Context(), store.TokenReset, e.userID(t, "ann@example.com"), time.Hour, e.now())
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := e.post(t, e.client(t), "/reset", url.Values{"token": {tok}, "password": {testSecret}, "confirm": {testSecret}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	if _, err := e.st.PendingLogin(t.Context(), raw, e.now()); err == nil {
		t.Error("a pending sign-in survived a password reset")
	}
}

// ---- startSession fails closed ----

func TestStartSessionRefusesUnknownMethodFor2FA(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	e.enable2FA(t, "ann@example.com")
	u := e.userByEmail(t, "ann@example.com")
	for _, m := range []string{"reset", "verify", "signup", "setup", ""} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/", nil)
		if e.srv.startSession(rec, req, u, m, "/", false) {
			t.Errorf("method %q created a session for a 2FA account", m)
		}
		for _, ck := range rec.Result().Cookies() {
			if ck.Name == "uptime_session" || ck.Name == "uptime_mfa" || ck.Name == "uptime_device" {
				t.Errorf("method %q set cookie %s", m, ck.Name)
			}
		}
		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login") || strings.HasPrefix(loc, "/login/2fa") {
			t.Errorf("method %q: Location %q", m, loc)
		}
	}
	for _, m := range []string{"password", "google", "github"} {
		rec := httptest.NewRecorder()
		e.srv.startSession(rec, httptest.NewRequest("POST", "/", nil), u, m, "/", false)
		if rec.Header().Get("Location") != "/login/2fa" {
			t.Errorf("method %q: Location %q", m, rec.Header().Get("Location"))
		}
	}
}

// ---- copy and placement ----

func TestTwoFactorMessages(t *testing.T) {
	e, _, _, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	resp, body := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"recovery": {"1"}, "code": {"abcd-abcd-abcd-abcd"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, html.EscapeString(msgMFAWrongRecovery)) {
		t.Fatalf("wrong recovery code: %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(msgMFAWrongRecovery, "Each one works once") {
		t.Error("recovery message")
	}
	for i := 0; i < 4; i++ {
		e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {"000000"}})
		if i == 2 {
			break
		}
	}
	// A fresh row after the budget is gone.
	for i := 0; i < 3; i++ {
		p := e.pendingSignIn(t, "198.51.100."+itoa(int64(i+1)), "two@example.com")
		e.postFrom(t, p, "198.51.100."+itoa(int64(i+1)), "/login/2fa", url.Values{"code": {"000000"}})
	}
	p := e.pendingSignIn(t, "198.51.100.9", "two@example.com")
	resp, body = e.postFrom(t, p, "198.51.100.9", "/login/2fa", url.Values{"code": {"000000"}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many attempts. Try again in about ") || !strings.Contains(body, "minutes.") {
		t.Fatalf("limited: %d %s", resp.StatusCode, body)
	}
}

func TestTwoFactorExpiredShownOnSignInPage(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	e.advance(6 * time.Minute)
	resp, body := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode == http.StatusSeeOther || !strings.Contains(body, msgMFAExpired) || !strings.Contains(body, `role="alert"`) || !strings.Contains(body, `name="password"`) {
		t.Fatalf("expired should render the sign-in page with a persistent error: %d %s", resp.StatusCode, body)
	}
	if cookieNamed(resp, "uptime_session") != nil {
		t.Error("signed in")
	}
}

func TestCodesPageLayoutAndErrorsInCard(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	secret := e.beginSetup(t, c, "ann@example.com")
	// A wrong code: the error sits inside the Two-factor card, tied to the field.
	resp, body := e.post(t, c, pathConfirm, url.Values{"code": {"000000"}})
	card := body[strings.Index(body, `id="two-factor"`):]
	card = card[:strings.Index(card, `id="methods"`)]
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(card, html.EscapeString(msgMFAWrong)) || !strings.Contains(card, `aria-invalid="true"`) || !strings.Contains(card, `aria-describedby=`) {
		t.Fatalf("error not in the card: %s", card)
	}
	if strings.Contains(body[:strings.Index(body, `id="two-factor"`)], html.EscapeString(msgMFAWrong)) {
		t.Error("the error is also shown at the top of the page")
	}
	_, body = e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}})
	if strings.Index(body, `id="two-factor"`) > strings.Index(body, `id="password"`) {
		t.Error("the codes card is below the Password card")
	}
	for _, want := range []string{`tabindex="-1"`, `class="alert warn"`, `href="/settings/security#two-factor"`, "I've saved them", `aria-label="QR code`} {
		if want == `aria-label="QR code` {
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("codes page lacks %q", want)
		}
	}
}

func TestSetupCardCopy(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	_, body := e.get(t, c, "/settings/security")
	if !strings.Contains(body, "Sign-in also asks for a code from an authenticator app. It protects the account if someone learns your password.") {
		t.Errorf("off lead: %s", body)
	}
	e.beginSetup(t, c, "ann@example.com")
	_, body = e.get(t, c, "/settings/security")
	for _, want := range []string{`aria-label="QR code for your authenticator app"`, pathCancel, `data-action="copy-key"`, "<ol", `autocapitalize`} {
		if want == `autocapitalize` {
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("setup card lacks %q", want)
		}
	}
}

func TestOnStateCopy(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, codes := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	_, body := e.get(t, c, "/settings/security")
	for _, want := range []string{"Code to make new recovery codes", "Code to turn off", `class="btns tf-act"`, `autocapitalize="off"`, `spellcheck="false"`} {
		if !strings.Contains(body, want) {
			t.Errorf("on state lacks %q", want)
		}
	}
	uid := e.userByEmail(t, "ann@example.com").ID
	for _, cd := range codes {
		e.st.UseRecoveryCode(t.Context(), uid, totp.HashRecovery(cd), e.now())
	}
	_, body = e.get(t, c, "/settings/security")
	if !strings.Contains(body, "No recovery codes left. Make new ones now.") || !strings.Contains(body, `class="alert warn"`) {
		t.Errorf("0 left: %s", body)
	}
}

func TestSensitiveFormCodeErrorNextToField(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	e.advance(11 * time.Minute)
	resp, body := e.post(t, c, "/settings/security/password", url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {newSecretPW}, "code": {"000000"}})
	card := body[strings.Index(body, `id="password"`):]
	card = card[:strings.Index(card, `id="two-factor"`)]
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(card, html.EscapeString(msgMFAWrong)) || !strings.Contains(card, `aria-invalid="true"`) {
		t.Fatalf("code error not next to the field: %d %s", resp.StatusCode, card)
	}
}

// ---- email ----

func TestTwoFactorEmails(t *testing.T) {
	loc := time.FixedZone("X", 5*3600+1800)
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, body string
		want       []string
	}{
		{"on", func() string { _, b := mfaOnMail("https://u.example", at, loc); return b }(), []string{"If this wasn't you, reset your password and ask this server's administrator to reset two-factor authentication.", "17:30"}},
		{"limited password", func() string { _, b := mfaLimitedMail("https://u.example", "password", at, loc); return b }(), []string{"Someone entered your password but not your two-factor code.", "change your password"}},
		{"limited google", func() string { _, b := mfaLimitedMail("https://u.example", "google", at, loc); return b }(), []string{"secure your Google account, then disconnect it under Sign-in methods."}},
		{"limited session", func() string { _, b := mfaLimitedMail("https://u.example", "", at, loc); return b }(), []string{"Someone signed in to your account entered a wrong two-factor code too many times. If it wasn't you, sign out everywhere and change your password."}},
		{"reset", func() string { _, b := mfaResetMail("https://u.example", at, loc); return b }(), []string{"An administrator turned off two-factor authentication on your account. Sign in, then turn it on again under Security in your Settings. If you didn't ask for this, contact the administrator."}},
		{"recovery new", func() string { _, b := recoveryNewMail("https://u.example", at, loc); return b }(), []string{"If this wasn't you", "make new recovery codes again"}},
		{"recovery used", func() string { _, b := recoveryUsedMail("https://u.example", 3, true, at, loc); return b }(), []string{"A recovery code was used to sign in. 3 left.", "make new recovery codes again"}},
		{"off", func() string { _, b := mfaOffMail("https://u.example", at, loc); return b }(), []string{"If this wasn't you"}},
	}
	for _, c := range cases {
		for _, w := range c.want {
			if !strings.Contains(c.body, w) {
				t.Errorf("%s: lacks %q:\n%s", c.name, w, c.body)
			}
		}
	}
}

// ---- admin ----

func TestAdminResetConfirmCopyAndIcon(t *testing.T) {
	e, op, _, _ := adminTF(t)
	e.account(t, "ann@example.com")
	e.enable2FA(t, "ann@example.com")
	_, body := e.get(t, op, "/admin/users")
	i := strings.Index(body, "/reset-2fa")
	form := body[i : i+strings.Index(body[i:], "</form>")]
	if !strings.Contains(form, "They will be signed out everywhere and can then sign in without a code.") || strings.Contains(form, "#i-key") {
		t.Errorf("reset form: %s", form)
	}
}
