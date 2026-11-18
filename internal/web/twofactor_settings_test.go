package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/totp"
)

const (
	pathSetup   = "/settings/security/2fa/setup"
	pathConfirm = "/settings/security/2fa/confirm"
	pathDisable = "/settings/security/2fa/disable"
	pathRegen   = "/settings/security/2fa/recovery"
)

var recoveryRe = regexp.MustCompile(`\b[a-z2-7]{4}(?:-[a-z2-7]{4}){3}\b`)

// signInWithCode signs email in through /login and /login/2fa, then moves the
// clock one step on so the next code is a fresh one.
func (e *env) signInWithCode(t *testing.T, ip, email, secret string) *http.Client {
	t.Helper()
	c := e.pendingSignIn(t, ip, email)
	if resp, body := e.postFrom(t, c, ip, "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("code step: %d %s", resp.StatusCode, body)
	}
	e.advance(31 * time.Second)
	return c
}

func (e *env) userByEmail(t *testing.T, email string) store.User {
	t.Helper()
	u, _, err := e.st.UserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// beginSetup runs the Set up step as a signed-in account and returns the pending secret.
func (e *env) beginSetup(t *testing.T, c *http.Client, email string) string {
	t.Helper()
	resp, body := e.post(t, c, pathSetup, url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup: %d %s", resp.StatusCode, body)
	}
	secret, err := e.st.PendingTOTP(t.Context(), e.userByEmail(t, email).ID, e.now())
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func TestTwoFactorSettingsSetupPage(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	_, body := e.get(t, c, "/settings/security")
	if !strings.Contains(body, pathSetup) || strings.Contains(body, "<svg class=\"qr") {
		t.Fatalf("off state lacks Set up: %s", body)
	}
	secret := e.beginSetup(t, c, "ann@example.com")
	resp, body := e.get(t, c, "/settings/security")
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("setup page: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	for _, want := range []string{`aria-label="QR code"`, groupSecret(secret), `name="code"`, pathConfirm, `autocomplete="one-time-code"`} {
		if !strings.Contains(body, want) {
			t.Errorf("setup page lacks %q", want)
		}
	}
	if strings.Contains(body, "otpauth://") {
		t.Error("the page carries the otpauth URI")
	}
	if !strings.Contains(body, "data-no-refresh") {
		t.Error("setup page can be replaced by a live refresh")
	}
	// A reload, and a second Set up, show the same secret.
	if again := e.beginSetup(t, c, "ann@example.com"); again != secret {
		t.Error("Set up minted a new secret while one was pending")
	}
	if _, body = e.get(t, c, "/settings/security"); !strings.Contains(body, groupSecret(secret)) {
		t.Error("reload changed the secret")
	}
	// The secret expires with its 15 minutes.
	e.advance(16 * time.Minute)
	if _, body = e.get(t, c, "/settings/security"); strings.Contains(body, groupSecret(secret)) {
		t.Error("expired secret still shown")
	}
}

func TestTwoFactorSettingsSetupNeedsRecentSignIn(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	e.advance(11 * time.Minute)
	resp, body := e.post(t, c, pathSetup, url.Values{})
	if resp.StatusCode == http.StatusSeeOther || !strings.Contains(body, msgBadCurrent) && !strings.Contains(body, "Current password") {
		t.Fatalf("stale setup without password: %d %s", resp.StatusCode, body)
	}
	if _, err := e.st.PendingTOTP(t.Context(), e.userByEmail(t, "ann@example.com").ID, e.now()); err == nil {
		t.Fatal("a pending secret was made without a recent sign-in")
	}
	if resp, _ = e.post(t, c, pathSetup, url.Values{"current": {"wrong password!"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	resp, body = e.post(t, c, pathSetup, url.Values{"current": {testSecret}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("with the password: %d %s", resp.StatusCode, body)
	}
	// The password counted as a fresh sign-in, so the page shows the setup.
	if _, body = e.get(t, c, "/settings/security"); !strings.Contains(body, `aria-label="QR code"`) {
		t.Error("setup state missing after the password")
	}
}

func TestTwoFactorSettingsSetupWhenOn(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	resp, body := e.post(t, c, pathSetup, url.Values{})
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, html.EscapeString(msgMFAAlreadyOn)) {
		t.Fatalf("setup while on: %d %s", resp.StatusCode, body)
	}
	if _, err := e.st.PendingTOTP(t.Context(), e.userByEmail(t, "ann@example.com").ID, e.now()); err == nil {
		t.Error("a pending secret exists next to an active one")
	}
}

func TestTwoFactorSettingsSocialOnlyNeedsRecentSignIn(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	c := o.client(t)
	o.run(t, c, "google", "")
	o.advance(11 * time.Minute)
	_, body := o.get(t, c, "/settings/security")
	if strings.Contains(body, pathSetup+`" `) && !strings.Contains(body, "sign in again with Google") {
		t.Errorf("social-only stale: Set up offered without a reauth link")
	}
	resp, _ := o.post(t, c, pathSetup, url.Values{})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("setup after a stale sign-in: %d", resp.StatusCode)
	}
}

func TestTwoFactorSettingsConfirm(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	other := e.signIn(t, "ann@example.com", testSecret)
	secret := e.beginSetup(t, c, "ann@example.com")

	// Wrong code: stays pending.
	resp, body := e.post(t, c, pathConfirm, url.Values{"code": {"000000"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, html.EscapeString(msgMFAWrong)) {
		t.Fatalf("wrong code: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, groupSecret(secret)) == false {
		t.Error("the setup state disappeared after a wrong code")
	}
	uid := e.userByEmail(t, "ann@example.com").ID
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), uid); enabled {
		t.Fatal("enabled by a wrong code")
	}
	// No code at all is not a try.
	if resp, _ = e.post(t, c, pathConfirm, url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("empty code: %d", resp.StatusCode)
	}

	// The right code enables it and shows the codes once.
	resp, body = e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(body, "data-no-refresh") {
		t.Fatalf("confirm: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	codes := recoveryRe.FindAllString(body, -1)
	if len(codes) != 10 {
		t.Fatalf("codes shown = %d, want 10:\n%s", len(codes), body)
	}
	seen := map[string]bool{}
	for _, cd := range codes {
		if seen[cd] {
			t.Errorf("duplicate code %s", cd)
		}
		seen[cd] = true
	}
	for _, want := range []string{`data-action="copy-codes"`, `data-action="download-codes"`, "will not be shown again"} {
		if !strings.Contains(body, want) {
			t.Errorf("codes page lacks %q", want)
		}
	}
	if strings.Contains(body, groupSecret(secret)) {
		t.Error("the secret is still on the codes page")
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), uid); !enabled {
		t.Fatal("not enabled")
	}
	if left, _ := e.st.RecoveryCodesLeft(t.Context(), uid); left != 10 {
		t.Errorf("codes stored = %d", left)
	}
	// Only hashes are stored.
	if ok, _ := e.st.UseRecoveryCode(t.Context(), uid, codes[0], e.now()); ok {
		t.Error("a plain recovery code matched a stored value")
	}
	// Other sessions are out, this one is verified.
	if resp, _ := e.get(t, other, "/settings/security"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("other session survived: %d", resp.StatusCode)
	}
	au, err := e.st.SessionAuth(t.Context(), jarCookie(c, "uptime_session").Value, e.now())
	if err != nil || au.MFAAt == nil {
		t.Fatalf("this session: %+v %v", au, err)
	}
	if countMail(e, msgMFAOnSubject) != 1 {
		t.Errorf("turned-on notices = %d", countMail(e, msgMFAOnSubject))
	}
	// The next view says how many are left and shows none.
	resp, body = e.get(t, c, "/settings/security")
	if !strings.Contains(body, "10 recovery codes left") || len(recoveryRe.FindAllString(body, -1)) != 0 {
		t.Errorf("on state: %v", recoveryRe.FindAllString(body, -1))
	}
	// Confirming again, with nothing pending, does nothing.
	e.advance(31 * time.Second)
	if resp, _ = e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}}); resp.StatusCode == 200 {
		t.Error("confirm accepted with nothing pending")
	}
}

func TestTwoFactorSettingsConfirmExpired(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	secret := e.beginSetup(t, c, "ann@example.com")
	e.advance(16 * time.Minute)
	resp, body := e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode == 200 || !strings.Contains(body, html.EscapeString(msgSetupExpired)) {
		t.Fatalf("expired: %d %s", resp.StatusCode, body)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), e.userByEmail(t, "ann@example.com").ID); enabled {
		t.Fatal("enabled from an expired secret")
	}
}

func TestTwoFactorSettingsConfirmUsesLimiter(t *testing.T) {
	e := newEnvMail(t, openCfg)
	c, _ := e.account(t, "ann@example.com")
	secret := e.beginSetup(t, c, "ann@example.com")
	for i := 0; i < 5; i++ {
		if resp, _ := e.post(t, c, pathConfirm, url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, resp.StatusCode)
		}
	}
	resp, body := e.post(t, c, pathConfirm, url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgMFALimited) {
		t.Fatalf("6th: %d %s", resp.StatusCode, body)
	}
}

func TestTwoFactorSettingsOnState(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, codes := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	uid := e.userByEmail(t, "ann@example.com").ID
	_, body := e.get(t, c, "/settings/security")
	for _, want := range []string{"10 recovery codes left", pathDisable, pathRegen} {
		if !strings.Contains(body, want) {
			t.Errorf("on state lacks %q", want)
		}
	}
	if strings.Contains(body, "Only 2 recovery codes left") || strings.Contains(body, "Running low") {
		t.Error("warning shown with 10 codes")
	}
	for i := 0; i < 8; i++ {
		if ok, _ := e.st.UseRecoveryCode(t.Context(), uid, totp.HashRecovery(codes[i]), e.now()); !ok {
			t.Fatal("cannot spend a code")
		}
	}
	_, body = e.get(t, c, "/settings/security")
	if !strings.Contains(body, "2 recovery codes left") || !strings.Contains(body, "Running low") {
		t.Errorf("no warning at 2 left: %s", body)
	}
	e.st.UseRecoveryCode(t.Context(), uid, totp.HashRecovery(codes[8]), e.now())
	if _, body = e.get(t, c, "/settings/security"); !strings.Contains(body, "1 recovery code left") {
		t.Error("singular wording missing")
	}
}

func TestTwoFactorSettingsDisable(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, codes := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	other := e.signInWithCode(t, "203.0.113.2", "ann@example.com", secret)
	uid := e.userByEmail(t, "ann@example.com").ID

	if resp, _ := e.post(t, c, pathDisable, url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no code: %d", resp.StatusCode)
	}
	resp, body := e.post(t, c, pathDisable, url.Values{"code": {"000000"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, html.EscapeString(msgMFAWrong)) {
		t.Fatalf("wrong code: %d %s", resp.StatusCode, body)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), uid); !enabled {
		t.Fatal("turned off by a wrong code")
	}
	// A recovery code turns it off too.
	resp, _ = e.post(t, c, pathDisable, url.Values{"code": {strings.ToUpper(codes[0])}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recovery code: %d", resp.StatusCode)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), uid); enabled {
		t.Fatal("still enabled")
	}
	if left, _ := e.st.RecoveryCodesLeft(t.Context(), uid); left != 0 {
		t.Errorf("recovery codes left = %d", left)
	}
	if resp, _ := e.get(t, other, "/settings/security"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("other session survived: %d", resp.StatusCode)
	}
	if resp, _ := e.get(t, c, "/settings/security"); resp.StatusCode != 200 {
		t.Errorf("this session was signed out: %d", resp.StatusCode)
	}
	if countMail(e, msgMFAOffSubject) != 1 {
		t.Errorf("turned-off notices = %d", countMail(e, msgMFAOffSubject))
	}
	// Off again: no second notice, no error page.
	e.advance(31 * time.Second)
	e.post(t, c, pathDisable, url.Values{"code": {e.code(t, secret)}})
	if countMail(e, msgMFAOffSubject) != 1 {
		t.Error("a notice was sent for turning off what was already off")
	}
	// The old step stays spent: the secret is gone, but last_step is kept.
	if ok, _ := e.st.AcceptTOTPStep(t.Context(), uid, secret, totp.Step(e.now())-100); ok {
		t.Error("accepted while off")
	}
}

func TestTwoFactorSettingsRegenerate(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, old := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	other := e.signInWithCode(t, "203.0.113.2", "ann@example.com", secret)
	uid := e.userByEmail(t, "ann@example.com").ID

	if resp, _ := e.post(t, c, pathRegen, url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	resp, body := e.post(t, c, pathRegen, url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("regenerate: %d", resp.StatusCode)
	}
	fresh := recoveryRe.FindAllString(body, -1)
	if len(fresh) != 10 {
		t.Fatalf("codes = %d", len(fresh))
	}
	for _, o := range old {
		for _, f := range fresh {
			if o == f {
				t.Fatal("a new code equals an old one")
			}
		}
		if ok, _ := e.st.UseRecoveryCode(t.Context(), uid, totp.HashRecovery(o), e.now()); ok {
			t.Fatal("an old recovery code still works")
		}
	}
	if ok, _ := e.st.UseRecoveryCode(t.Context(), uid, totp.HashRecovery(fresh[0]), e.now()); !ok {
		t.Error("a new recovery code does not work")
	}
	if resp, _ := e.get(t, other, "/settings/security"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("other session survived: %d", resp.StatusCode)
	}
	if countMail(e, msgRecoveryNewSubject) != 1 {
		t.Errorf("regenerate notices = %d", countMail(e, msgRecoveryNewSubject))
	}
	// Only for an account that has two-factor on.
	e.account(t, "bob@example.com")
	bob := e.signIn(t, "bob@example.com", testSecret)
	if resp, _ := e.post(t, bob, pathRegen, url.Values{"code": {"123456"}}); resp.StatusCode == 200 {
		t.Error("regenerate worked without two-factor")
	}
}

// ---- recent verification ----

func TestRecentVerifiedNeedsCodeForSensitiveForms(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)

	// Right after sign-in the session is verified: no code field, no code needed.
	_, body := e.get(t, c, "/settings/security")
	if strings.Contains(body, "Two-factor code") {
		t.Error("a code field is shown right after sign-in")
	}
	e.advance(11 * time.Minute)
	_, body = e.get(t, c, "/settings/security")
	if !strings.Contains(body, "Two-factor code") {
		t.Fatalf("no code field after 11 minutes: %s", body)
	}
	pw := url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {newSecretPW}}
	// The password alone is not enough.
	resp, body := e.post(t, c, "/settings/security/password", pw)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, html.EscapeString(msgCodeRequired)) {
		t.Fatalf("password without code: %d %s", resp.StatusCode, body)
	}
	if _, err := e.hash.Verify(t.Context(), mustHash(t, e, e.userByEmail(t, "ann@example.com").ID), testSecret); err != nil {
		t.Fatal(err)
	}
	// A password re-entry, which sets auth_at, does not help either.
	au, _ := e.st.SessionAuth(t.Context(), jarCookie(c, "uptime_session").Value, e.now())
	e.st.MarkReauth(t.Context(), au.SessionHash, e.now())
	if resp, _ = e.post(t, c, "/settings/security/password", pw); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("after reauth: %d", resp.StatusCode)
	}
	// Code plus password works, and then no code is needed for ten minutes.
	pw.Set("code", e.code(t, secret))
	if resp, body = e.post(t, c, "/settings/security/password", pw); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("with code: %d %s", resp.StatusCode, body)
	}
	e.advance(9 * time.Minute)
	pw2 := url.Values{"current": {newSecretPW}, "new": {testSecret}, "confirm": {testSecret}}
	if resp, body = e.post(t, c, "/settings/security/password", pw2); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("within ten minutes of the code: %d %s", resp.StatusCode, body)
	}
	e.advance(2 * time.Minute)
	if resp, _ = e.post(t, c, "/settings/security/password", url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {newSecretPW}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("after ten minutes: %d", resp.StatusCode)
	}
}

func mustHash(t *testing.T, e *env, id int64) string {
	t.Helper()
	phc, err := e.st.PasswordHash(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return phc
}

func TestRecentVerifiedConnectDisconnectDelete(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	e.advance(11 * time.Minute)

	// Delete: password alone is refused.
	resp, body := e.post(t, c, "/settings/danger/delete", url.Values{"current": {testSecret}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, html.EscapeString(msgCodeRequired)) {
		t.Fatalf("delete without code: %d %s", resp.StatusCode, body)
	}
	if !e.userExists(t, "ann@example.com") {
		t.Fatal("deleted without a code")
	}
	// A wrong code is refused as a wrong code.
	if resp, _ = e.post(t, c, "/settings/danger/delete", url.Values{"current": {testSecret}, "code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong code: %d", resp.StatusCode)
	}
	// A wrong password is still refused before the code is tried.
	if resp, _ = e.post(t, c, "/settings/danger/delete", url.Values{"current": {"not the password"}, "code": {e.code(t, secret)}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("wrong password: %d", resp.StatusCode)
	}
	// Connect: the code and password start the flow.
	resp, _ = e.post(t, c, "/settings/security/identities/google/connect", url.Values{"current": {testSecret}})
	if resp.StatusCode == http.StatusSeeOther && strings.HasPrefix(resp.Header.Get("Location"), "/auth/") {
		t.Error("connect started without a code")
	}
	// Delete with password and code.
	resp, body = e.post(t, c, "/settings/danger/delete", url.Values{"current": {testSecret}, "code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther || e.userExists(t, "ann@example.com") {
		t.Fatalf("delete with code: %d %s", resp.StatusCode, body)
	}
}

func TestRecentVerifiedSocialOnlyTwoFactor(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.cfg.TrustProxy = true
	o.social(t, "person@example.com", "google", "12345")
	secret, _ := o.enable2FA(t, "person@example.com")
	c := o.client(t)
	cb, _ := o.begin(t, c, "google", "")
	o.fetch(t, c, cb)
	o.postFrom(t, c, "203.0.113.5", "/login/2fa", url.Values{"code": {o.code(t, secret)}})
	o.advance(11 * time.Minute)
	// A provider re-sign-in does not stand in for the code; the code field is on the form.
	_, body := o.get(t, c, "/settings/danger")
	if !strings.Contains(body, "Two-factor code") || strings.Contains(body, "sign in again with Google") {
		t.Errorf("danger zone for a social-only 2FA account: %s", body)
	}
	resp, _ := o.post(t, c, "/settings/danger/delete", url.Values{})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("delete without code: %d", resp.StatusCode)
	}
	resp, _ = o.post(t, c, "/settings/danger/delete", url.Values{"code": {o.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther || o.userExists(t, "person@example.com") {
		t.Errorf("delete with code: %d", resp.StatusCode)
	}
}

func TestRecentVerifiedMFAAtNotAuthAt(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	e.enable2FA(t, "ann@example.com")
	a := &authCtx{}
	a.User.TOTPEnabledAt = &time.Time{}
	now := e.now()
	a.AuthAt = &now
	if e.srv.recentVerified(a) {
		t.Error("auth_at alone verified a 2FA account")
	}
	a.MFAAt = &now
	if !e.srv.recentVerified(a) {
		t.Error("fresh mfa_at not accepted")
	}
	old := now.Add(-11 * time.Minute)
	a.MFAAt = &old
	if e.srv.recentVerified(a) {
		t.Error("stale mfa_at accepted")
	}
	a.User.TOTPEnabledAt = nil
	a.AuthAt = &now
	if !e.srv.recentVerified(a) {
		t.Error("an account without 2FA lost the auth_at rule")
	}
}

// ---- the limiter is one budget ----

func TestLimiterSharedWithSettings(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.cfg.TrustProxy = true
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	// Three failures at the sign-in code page, two at turn-off...
	for i := 0; i < 3; i++ {
		p := e.pendingSignIn(t, "198.51.100."+itoa(int64(i+1)), "ann@example.com")
		if resp, _ := e.postFrom(t, p, "198.51.100."+itoa(int64(i+1)), "/login/2fa", url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("sign-in failure %d: %d", i+1, resp.StatusCode)
		}
	}
	for i := 0; i < 2; i++ {
		if resp, _ := e.post(t, c, pathDisable, url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("turn-off failure %d: %d", i+1, resp.StatusCode)
		}
	}
	// ... and the budget is spent everywhere, even for the right code.
	resp, body := e.post(t, c, pathDisable, url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgMFALimited) {
		t.Fatalf("turn-off after 5 shared failures: %d %s", resp.StatusCode, body)
	}
	if resp, _ = e.post(t, c, pathRegen, url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("regenerate: %d", resp.StatusCode)
	}
	p := e.pendingSignIn(t, "198.51.100.77", "ann@example.com")
	if resp, _ = e.postFrom(t, p, "198.51.100.77", "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("sign-in: %d", resp.StatusCode)
	}
	if n := countMail(e, msgMFALimitedSubject); n != 1 {
		t.Errorf("notices = %d, want 1", n)
	}
}

// §12: a URI too long for the encoder shows the typed key and says so.
func TestTwoFactorSetupWithoutQR(t *testing.T) {
	e := newEnvMail(t, openCfg)
	email := strings.Repeat("a", 60) + "@" + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + ".example"
	phc, err := e.hash.Hash(t.Context(), testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.st.CreateAccount(t.Context(), store.AccountInput{Email: email, PasswordHash: phc, WorkspaceName: "w", Slug: "long", Verified: true, Now: e.now()}); err != nil {
		t.Fatal(err)
	}
	c := e.signIn(t, email, testSecret)
	secret := e.beginSetup(t, c, email)
	_, body := e.get(t, c, "/settings/security")
	if strings.Contains(body, `aria-label="QR code"`) || !strings.Contains(body, groupSecret(secret)) || !strings.Contains(body, "QR code does not fit") || strings.Contains(body, "Scan the QR code") {
		t.Errorf("long address: %s", body)
	}
}

// A code can be a recovery code, so no field that takes one is digits-only.
func TestTwoFactorCodeFieldsAcceptRecoveryCodes(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	_, body := e.get(t, c, "/settings/security")
	for _, f := range []string{`id="tf-off"`, `id="tf-regen"`} {
		i := strings.Index(body, f)
		if i < 0 {
			t.Fatalf("%s missing", f)
		}
		if tag := body[i : i+strings.Index(body[i:], ">")]; strings.Contains(tag, "inputmode") {
			t.Errorf("%s is digits-only: %s", f, tag)
		}
	}
	e.advance(11 * time.Minute)
	_, body = e.get(t, c, "/settings/security")
	if i := strings.Index(body, `id="acc-code"`); i < 0 || strings.Contains(body[i:i+strings.Index(body[i:], ">")], "inputmode") {
		t.Error("the sensitive-form code field is digits-only")
	}
}

// The OAuth start of a link flow is gated by recentVerified, not auth_at.
func TestLinkStartNeedsFreshCode(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.cfg.TrustProxy = true
	o.social(t, "person@example.com", "google", "12345")
	secret, _ := o.enable2FA(t, "person@example.com")
	c := o.client(t)
	cb, _ := o.begin(t, c, "google", "")
	o.fetch(t, c, cb)
	o.postFrom(t, c, "203.0.113.5", "/login/2fa", url.Values{"code": {o.code(t, secret)}})
	if resp := o.start(t, c, "google", "intent=link"); resp.StatusCode == http.StatusForbidden {
		t.Fatalf("link start refused right after the code: %d", resp.StatusCode)
	}
	o.advance(11 * time.Minute)
	au, err := o.st.SessionAuth(t.Context(), jarCookie(c, "uptime_session").Value, o.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.st.MarkReauth(t.Context(), au.SessionHash, o.now()); err != nil { // a fresh auth_at, stale mfa_at
		t.Fatal(err)
	}
	if resp := o.start(t, c, "google", "intent=link"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("link start with a fresh auth_at but a stale mfa_at: %d, want 403", resp.StatusCode)
	}
}

func TestDisconnectNeedsCode(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	uid := e.userID(t, "ann@example.com")
	if err := e.st.LinkIdentity(t.Context(), uid, "google", "sub-1", "ann@example.com", e.now()); err != nil {
		t.Fatal(err)
	}
	secret, _ := e.enable2FA(t, "ann@example.com")
	c := e.signInWithCode(t, "203.0.113.1", "ann@example.com", secret)
	e.advance(11 * time.Minute)
	const path = "/settings/security/identities/google/delete"
	resp, body := e.post(t, c, path, url.Values{"current": {testSecret}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, html.EscapeString(msgCodeRequired)) {
		t.Fatalf("disconnect without a code: %d %s", resp.StatusCode, body)
	}
	if ids, _ := e.st.Identities(t.Context(), uid); len(ids) != 1 {
		t.Fatal("disconnected without a code")
	}
	resp, _ = e.post(t, c, path, url.Values{"current": {testSecret}, "code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("with a code: %d", resp.StatusCode)
	}
	if ids, _ := e.st.Identities(t.Context(), uid); len(ids) != 0 {
		t.Error("not disconnected with a code")
	}
}
