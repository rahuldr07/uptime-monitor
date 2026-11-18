package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/totp"
)

// enable2FA turns two-factor on for email directly in the store and returns
// the secret and the recovery codes. The replay guard starts ten steps back,
// so a code for the current step works at once. Every session of the user is
// revoked, as EnableTOTP does.
func (e *env) enable2FA(t *testing.T, email string) (secret string, codes []string) {
	t.Helper()
	u, _, err := e.st.UserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	if secret, err = totp.NewSecret(); err != nil {
		t.Fatal(err)
	}
	if _, err = e.st.BeginTOTP(t.Context(), u.ID, secret, e.now()); err != nil {
		t.Fatal(err)
	}
	if codes, err = totp.NewRecoveryCodes(10); err != nil {
		t.Fatal(err)
	}
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = totp.HashRecovery(c)
	}
	if err = e.st.EnableTOTP(t.Context(), u.ID, secret, totp.Step(e.now())-10, hashes, "", e.now()); err != nil {
		t.Fatal(err)
	}
	return secret, codes
}

// code is the authenticator code for secret right now.
func (e *env) code(t *testing.T, secret string) string {
	t.Helper()
	c, err := totp.Code(secret, totp.Step(e.now()))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// postFrom posts a form as if from ip (needs trust_proxy).
func (e *env) postFrom(t *testing.T, c *http.Client, ip, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := readBody(resp)
	return resp, b
}

// pendingSignIn signs email in with the password and returns the client that
// now waits at /login/2fa.
func (e *env) pendingSignIn(t *testing.T, ip, email string) *http.Client {
	t.Helper()
	c := e.client(t)
	resp, body := e.postFrom(t, c, ip, "/login", url.Values{"email": {email}, "password": {testSecret}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("password step: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	return c
}

func twoFA(t *testing.T) (*env, *http.Client, string, []string) {
	t.Helper()
	e := newEnvMail(t, openCfg)
	e.cfg.TrustProxy = true
	e.account(t, "two@example.com")
	secret, codes := e.enable2FA(t, "two@example.com")
	return e, nil, secret, codes
}

func countMail(e *env, subject string) int {
	n := 0
	for _, m := range e.mail.all() {
		if strings.Contains(m.Subject, subject) {
			n++
		}
	}
	return n
}

// ---- the one session path ----

// TestCreateSessionOnlyInStartSession fails if a session is created anywhere
// but startSession, which is where 2FA is enforced.
func TestCreateSessionOnlyInStartSession(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, _ := os.ReadFile(f)
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && (sel.Sel.Name == "CreateSession" || sel.Sel.Name == "CreateSessionMFA") {
					found++
					if funcName(fd) != "(*Server).startSession" {
						t.Errorf("%s: %s creates a session outside startSession", f, fset.Position(sel.Pos()))
					}
				}
				return true
			})
		}
	}
	if found == 0 {
		t.Error("no session creation found; the test is broken")
	}
}

// ---- password sign-in ----

func TestTwoFactorPasswordSignIn(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	resp, _ := e.get(t, c, "/login/2fa")
	if resp.StatusCode != 200 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if jarCookie(c, "uptime_session") != nil || jarCookie(c, "uptime_device") != nil {
		t.Fatal("session or device cookie before the code")
	}
	if jarCookie(c, "uptime_mfa") == nil {
		t.Fatal("no pending cookie")
	}
	if resp, _ := e.get(t, c, "/settings/security"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("settings before the code: %d", resp.StatusCode)
	}
	resp, body := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("code step: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if cookieNamed(resp, "uptime_session") == nil || cookieNamed(resp, "uptime_device") == nil {
		t.Errorf("session and device cookies expected: %v", resp.Cookies())
	}
	if ck := cookieNamed(resp, "uptime_mfa"); ck == nil || ck.MaxAge >= 0 {
		t.Errorf("pending cookie not cleared: %+v", ck)
	}
	au, err := e.st.SessionAuth(t.Context(), jarCookie(c, "uptime_session").Value, e.now())
	if err != nil || au.MFAAt == nil || au.AuthAt == nil {
		t.Fatalf("session: %+v %v", au, err)
	}
	if resp, _ := e.get(t, c, "/settings/security"); resp.StatusCode != 200 {
		t.Fatalf("signed in: %d", resp.StatusCode)
	}
}

func TestTwoFactorCodeFormatting(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	code := e.code(t, secret)
	resp, _ := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {" " + code[:3] + " " + code[3:] + " "}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("spaced code: %d", resp.StatusCode)
	}
}

func TestTwoFactorNextCarried(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.client(t)
	resp, _ := e.postFrom(t, c, "203.0.113.1", "/login", url.Values{"email": {"two@example.com"}, "password": {testSecret}, "next": {"/targets"}})
	if resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.Header.Get("Location") != "/targets" {
		t.Errorf("next = %q", resp.Header.Get("Location"))
	}
}

func TestTwoFactorWrongCode(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	resp, body := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {"000000"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, html.EscapeString(msgMFAWrong)) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if jarCookie(c, "uptime_session") != nil {
		t.Error("session after a wrong code")
	}
	// Letters and a long code are plain wrong codes, not errors.
	for _, bad := range []string{"abcdef", strings.Repeat("9", 400)} {
		resp, body = e.postFrom(t, c, "203.0.113.2", "/login/2fa", url.Values{"code": {bad}})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%.8q: %d %s", bad, resp.StatusCode, body)
		}
	}
	// The row survives wrong codes up to its cap; the right one still works.
	resp, _ = e.postFrom(t, c, "203.0.113.3", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("right code after wrong ones: %d", resp.StatusCode)
	}
}

func TestTwoFactorRecoveryCode(t *testing.T) {
	e, _, _, codes := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	resp, body := e.get(t, c, "/login/2fa?recovery=1")
	if resp.StatusCode != 200 || !strings.Contains(body, `name="recovery" value="1"`) || strings.Contains(body, "one-time-code") {
		t.Fatalf("recovery page: %d %s", resp.StatusCode, body)
	}
	if _, body = e.get(t, c, "/login/2fa"); !strings.Contains(body, "one-time-code") || !strings.Contains(body, `href="/login/2fa?recovery=1"`) {
		t.Errorf("code page lacks the recovery link or one-time-code: %s", body)
	}
	spaced := strings.ToUpper(strings.ReplaceAll(codes[0], "-", " "))
	resp, body = e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"recovery": {"1"}, "code": {spaced}})
	if resp.StatusCode != http.StatusSeeOther || cookieNamed(resp, "uptime_session") == nil {
		t.Fatalf("recovery sign-in: %d %s", resp.StatusCode, body)
	}
	if n := countMail(e, msgRecoveryUsedSubject); n != 1 {
		t.Fatalf("recovery notices = %d", n)
	}
	for _, m := range e.mail.all() {
		if strings.Contains(m.Subject, msgRecoveryUsedSubject) && !strings.Contains(m.Body, "9 left") {
			t.Errorf("notice lacks the count: %q", m.Body)
		}
		if strings.Contains(m.Body, codes[0]) {
			t.Error("notice contains the recovery code")
		}
	}
	// Once only.
	c2 := e.pendingSignIn(t, "203.0.113.4", "two@example.com")
	resp, _ = e.postFrom(t, c2, "203.0.113.4", "/login/2fa", url.Values{"recovery": {"1"}, "code": {codes[0]}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("reused recovery code: %d", resp.StatusCode)
	}
	// A TOTP code is not a recovery code and vice versa.
	resp, _ = e.postFrom(t, c2, "203.0.113.4", "/login/2fa", url.Values{"code": {strings.ReplaceAll(codes[1], "-", "")}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("recovery code in the code field: %d", resp.StatusCode)
	}
	// Over-long recovery input is refused as a wrong code.
	resp, _ = e.postFrom(t, c2, "203.0.113.5", "/login/2fa", url.Values{"recovery": {"1"}, "code": {strings.Repeat("a", 5000)}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("long recovery input: %d", resp.StatusCode)
	}
}

func TestTwoFactorCancel(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	raw := jarCookie(c, "uptime_mfa").Value
	resp, _ := e.postFrom(t, c, "203.0.113.1", "/login/2fa/cancel", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("cancel: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if ck := cookieNamed(resp, "uptime_mfa"); ck == nil || ck.MaxAge >= 0 {
		t.Errorf("cookie not cleared: %+v", ck)
	}
	if _, err := e.st.PendingLogin(t.Context(), raw, e.now()); err == nil {
		t.Error("row survived cancel")
	}
	// Replaying the old cookie with the right code gets nothing.
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/login/2fa", strings.NewReader(url.Values{"code": {e.code(t, secret)}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "uptime_mfa", Value: raw})
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || strings.HasPrefix(resp.Header.Get("Location"), "/login/2fa") || cookieNamed(resp, "uptime_session") != nil {
		t.Errorf("replay after cancel: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestTwoFactorExpiredAndTampered(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	e.advance(6 * time.Minute)
	resp, _ := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther || cookieNamed(resp, "uptime_session") != nil {
		t.Fatalf("expired: %d", resp.StatusCode)
	}
	_, body := e.get(t, c, resp.Header.Get("Location"))
	if !strings.Contains(body, msgMFAExpired) {
		t.Errorf("expired message missing: %s", body)
	}
	if resp, _ := e.get(t, c, "/login/2fa"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("GET with a dead row: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A tampered or missing cookie goes back to sign-in.
	c2 := e.client(t)
	c2.Jar.SetCookies(nil, []*http.Cookie{{Name: "uptime_mfa", Value: "tampered"}})
	for _, f := range []func() *http.Response{
		func() *http.Response { r, _ := e.get(t, c2, "/login/2fa"); return r },
		func() *http.Response {
			r, _ := e.postFrom(t, c2, "203.0.113.9", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
			return r
		},
		func() *http.Response { r, _ := e.get(t, e.client(t), "/login/2fa"); return r },
	} {
		r := f()
		if r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login") || cookieNamed(r, "uptime_session") != nil {
			t.Errorf("tampered: %d %q", r.StatusCode, r.Header.Get("Location"))
		}
	}
}

func TestTwoFactorTriesCap(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	for i := 0; i < store.PendingLoginTries; i++ {
		resp, _ := e.postFrom(t, c, "203.0.113."+string(rune('1'+i)), "/login/2fa", url.Values{"code": {"000000"}})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("try %d: %d", i+1, resp.StatusCode)
		}
	}
	e.advance(31 * time.Second) // a fresh step, and still far from any limit
	resp, body := e.postFrom(t, c, "203.0.113.20", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode == http.StatusSeeOther && cookieNamed(resp, "uptime_session") != nil {
		t.Fatal("6th try on one row signed in")
	}
	if !strings.Contains(body, msgMFALimited) && !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Errorf("6th try: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
}

func TestPendingInvalidatedByReset(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	if err := e.st.DisableTOTP(t.Context(), e.userID(t, "two@example.com"), ""); err != nil {
		t.Fatal(err)
	}
	resp, _ := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther || strings.HasPrefix(resp.Header.Get("Location"), "/login/2fa") || cookieNamed(resp, "uptime_session") != nil {
		t.Fatalf("after reset: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if jarCookie(c, "uptime_session") != nil {
		t.Error("signed in without a code")
	}
}

func TestPendingInvalidatedByDisable(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	e.disable(t, "two@example.com")
	resp, _ := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if cookieNamed(resp, "uptime_session") != nil || strings.HasPrefix(resp.Header.Get("Location"), "/login/2fa") {
		t.Fatalf("disabled account: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestTwoFactorSameCodeTwoTabs(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	a := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	b := e.pendingSignIn(t, "203.0.113.2", "two@example.com")
	code := e.code(t, secret)
	var wg sync.WaitGroup
	got := make([]*http.Response, 2)
	for i, c := range []*http.Client{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = e.postFrom(t, c, "203.0.113."+string(rune('3'+i)), "/login/2fa", url.Values{"code": {code}})
		}()
	}
	wg.Wait()
	ok := 0
	for _, r := range got {
		if r.StatusCode == http.StatusSeeOther && cookieNamed(r, "uptime_session") != nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("%d of 2 tabs signed in with one code, want 1 (%d, %d)", ok, got[0].StatusCode, got[1].StatusCode)
	}
}

// A used code is also refused on a later sign-in.
func TestTwoFactorReplayRefused(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	code := e.code(t, secret)
	c := e.pendingSignIn(t, "203.0.113.1", "two@example.com")
	if resp, _ := e.postFrom(t, c, "203.0.113.1", "/login/2fa", url.Values{"code": {code}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use: %d", resp.StatusCode)
	}
	c2 := e.pendingSignIn(t, "203.0.113.2", "two@example.com")
	if resp, _ := e.postFrom(t, c2, "203.0.113.2", "/login/2fa", url.Values{"code": {code}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay: %d", resp.StatusCode)
	}
	e.advance(30 * time.Second)
	if resp, _ := e.postFrom(t, c2, "203.0.113.2", "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("next step: %d", resp.StatusCode)
	}
}

// ---- the shared limiter ----

func TestLimiterSharedAcrossEntryPoints(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	// A device cookie that is valid for this user must not matter.
	dev := &http.Cookie{Name: "uptime_device", Value: "x"}
	uid := e.userID(t, "two@example.com")
	dev.Value = itoa(uid) + "." + e.srv.deviceMAC(uid)
	for i := 0; i < 5; i++ {
		c := e.pendingSignIn(t, "198.51.100."+itoa(int64(i+1)), "two@example.com")
		c.Jar.SetCookies(nil, []*http.Cookie{dev})
		resp, _ := e.postFrom(t, c, "198.51.100."+itoa(int64(i+1)), "/login/2fa", url.Values{"code": {"000000"}})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i+1, resp.StatusCode)
		}
	}
	// The 6th, with the right code, on a fresh row and a fresh address.
	c := e.pendingSignIn(t, "198.51.100.50", "two@example.com")
	c.Jar.SetCookies(nil, []*http.Cookie{dev})
	resp, body := e.postFrom(t, c, "198.51.100.50", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgMFALimited) {
		t.Fatalf("6th: %d %s", resp.StatusCode, body)
	}
	if cookieNamed(resp, "uptime_session") != nil {
		t.Fatal("signed in while limited")
	}
	if n := countMail(e, msgMFALimitedSubject); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	// Still limited, still one notice.
	c = e.pendingSignIn(t, "198.51.100.51", "two@example.com")
	if resp, _ = e.postFrom(t, c, "198.51.100.51", "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("still limited: %d", resp.StatusCode)
	}
	if n := countMail(e, msgMFALimitedSubject); n != 1 {
		t.Fatalf("notices = %d after more attempts, want 1", n)
	}
	// 16 minutes later the short window is clear.
	e.advance(16 * time.Minute)
	c = e.pendingSignIn(t, "198.51.100.52", "two@example.com")
	if resp, _ = e.postFrom(t, c, "198.51.100.52", "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("after the window: %d", resp.StatusCode)
	}
}

func TestLimiterDailyCap(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	n := int64(0)
	for batch := 0; batch < 4; batch++ {
		for i := 0; i < 5; i++ {
			n++
			c := e.pendingSignIn(t, "192.0.2."+itoa(n), "two@example.com")
			if resp, _ := e.postFrom(t, c, "192.0.2."+itoa(n), "/login/2fa", url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("failure %d: %d", n, resp.StatusCode)
			}
		}
		e.advance(16 * time.Minute)
	}
	// 20 failures in 64 minutes: the 15-minute window is clear, the day is not.
	c := e.pendingSignIn(t, "192.0.2.200", "two@example.com")
	resp, body := e.postFrom(t, c, "192.0.2.200", "/login/2fa", url.Values{"code": {e.code(t, secret)}})
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, msgMFALimited) {
		t.Fatalf("21st in a day: %d %s", resp.StatusCode, body)
	}
	if n := countMail(e, msgMFALimitedSubject); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
	e.advance(24 * time.Hour)
	c = e.pendingSignIn(t, "192.0.2.201", "two@example.com")
	if resp, _ = e.postFrom(t, c, "192.0.2.201", "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("next day: %d", resp.StatusCode)
	}
}

// Successful codes never use the budget up.
func TestLimiterRefundsSuccess(t *testing.T) {
	e, _, secret, _ := twoFA(t)
	for i := 0; i < 8; i++ {
		ip := "192.0.2." + itoa(int64(i+1))
		c := e.pendingSignIn(t, ip, "two@example.com")
		if resp, _ := e.postFrom(t, c, ip, "/login/2fa", url.Values{"code": {e.code(t, secret)}}); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("sign-in %d: %d", i+1, resp.StatusCode)
		}
		e.advance(30 * time.Second)
	}
}

// ---- other sign-in methods ----

func TestOAuthSignInAsksForCode(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.cfg.TrustProxy = true
	u := o.social(t, "person@example.com", "google", "12345")
	secret, _ := o.enable2FA(t, "person@example.com")
	other, _ := o.account(t, "other@example.com")
	oldRaw := jarCookie(other, "uptime_session").Value

	cb, _ := o.begin(t, other, "google", "next=%2Ftargets")
	resp, _ := o.fetch(t, other, cb)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login/2fa" {
		t.Fatalf("callback: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cookieNamed(resp, "uptime_session") != nil || cookieNamed(resp, "uptime_device") != nil {
		t.Fatalf("session or device cookie before the code: %v", resp.Cookies())
	}
	if _, err := o.st.SessionAuth(t.Context(), oldRaw, o.now()); err != nil {
		t.Fatalf("previous session did not survive the pending step: %v", err)
	}
	if !signedIn(t, o, other) {
		t.Error("previous session lost")
	}
	resp, body := o.postFrom(t, other, "203.0.113.7", "/login/2fa", url.Values{"code": {o.code(t, secret)}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/targets" {
		t.Fatalf("code step: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if cookieNamed(resp, "uptime_session") == nil || cookieNamed(resp, "uptime_device") == nil {
		t.Errorf("cookies after the code: %v", resp.Cookies())
	}
	if _, err := o.st.SessionAuth(t.Context(), oldRaw, o.now()); err == nil {
		t.Error("previous session survived the code step")
	}
	au, err := o.st.SessionAuth(t.Context(), jarCookie(other, "uptime_session").Value, o.now())
	if err != nil || au.User.ID != u.ID || au.MFAAt == nil {
		t.Fatalf("new session: %+v %v", au, err)
	}
	logs := o.logs.String()
	if strings.Count(logs, "oauth_login_ok") != 1 {
		t.Errorf("oauth_login_ok logged %d times:\n%s", strings.Count(logs, "oauth_login_ok"), logs)
	}
}

func TestOAuthSignInNoLogBeforeCode(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	o.enable2FA(t, "person@example.com")
	c := o.client(t)
	o.run(t, c, "google", "")
	if strings.Contains(o.logs.String(), "oauth_login_ok") {
		t.Error("oauth_login_ok logged before a session exists")
	}
}

func TestResetDoesNotSignIn2FAAccount(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "two@example.com")
	e.enable2FA(t, "two@example.com")
	tok, err := e.st.CreateToken(t.Context(), store.TokenReset, e.userID(t, "two@example.com"), time.Hour, e.now())
	if err != nil {
		t.Fatal(err)
	}
	c := e.client(t)
	resp, _ := e.post(t, c, "/reset", url.Values{"token": {tok}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("reset: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if cookieNamed(resp, "uptime_session") != nil || cookieNamed(resp, "uptime_device") != nil {
		t.Fatal("reset signed in a 2FA account")
	}
	_, body := e.get(t, c, resp.Header.Get("Location"))
	if !strings.Contains(body, msgPasswordChangedSignIn) {
		t.Errorf("login page lacks the message: %s", body)
	}
	// The new password still needs the code.
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {"two@example.com"}, "password": {newSecretPW}}); resp.Header.Get("Location") != "/login/2fa" {
		t.Errorf("sign-in after reset: %q", resp.Header.Get("Location"))
	}
}

func TestVerifyDoesNotSignIn2FAAccount(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.addUnverified(t, "two@example.com")
	e.enable2FA(t, "two@example.com")
	tok, err := e.st.CreateToken(t.Context(), store.TokenVerify, e.userID(t, "two@example.com"), time.Hour, e.now())
	if err != nil {
		t.Fatal(err)
	}
	c := e.client(t)
	resp, _ := e.post(t, c, "/verify", url.Values{"token": {tok}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") || cookieNamed(resp, "uptime_session") != nil {
		t.Fatalf("verify: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Someone who stops at exactly the cap is never refused, but the owner is
// still told: the warning comes with the failure that fills the window.
func TestLimiterNoticeAtTheCap(t *testing.T) {
	e, _, _, _ := twoFA(t)
	for i := 0; i < 4; i++ {
		c := e.pendingSignIn(t, "198.51.100."+itoa(int64(i+1)), "two@example.com")
		e.postFrom(t, c, "198.51.100."+itoa(int64(i+1)), "/login/2fa", url.Values{"code": {"000000"}})
	}
	if n := countMail(e, msgMFALimitedSubject); n != 0 {
		t.Fatalf("notice after 4 failures: %d", n)
	}
	c := e.pendingSignIn(t, "198.51.100.5", "two@example.com")
	if resp, _ := e.postFrom(t, c, "198.51.100.5", "/login/2fa", url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("5th failure: %d", resp.StatusCode)
	}
	if n := countMail(e, msgMFALimitedSubject); n != 1 {
		t.Fatalf("notices after exactly 5 failures = %d, want 1", n)
	}
	// Nothing more is sent for further refusals.
	c = e.pendingSignIn(t, "198.51.100.6", "two@example.com")
	e.postFrom(t, c, "198.51.100.6", "/login/2fa", url.Values{"code": {"000000"}})
	if n := countMail(e, msgMFALimitedSubject); n != 1 {
		t.Fatalf("notices = %d, want 1", n)
	}
}

// The daily window warns when its 20th slot goes too.
func TestLimiterNoticeAtTheDailyCap(t *testing.T) {
	e, _, _, _ := twoFA(t)
	n := int64(0)
	for batch := 0; batch < 4; batch++ {
		for i := 0; i < 5; i++ {
			n++
			c := e.pendingSignIn(t, "192.0.2."+itoa(n), "two@example.com")
			e.postFrom(t, c, "192.0.2."+itoa(n), "/login/2fa", url.Values{"code": {"000000"}})
		}
		e.advance(16 * time.Minute)
	}
	if got := countMail(e, msgMFALimitedSubject); got != 1 {
		t.Fatalf("notices = %d, want 1 (the 15-minute window warned on day one)", got)
	}
}
