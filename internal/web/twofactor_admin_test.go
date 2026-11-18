package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// adminTF is an env whose operator has two-factor on and is signed in with a code.
func adminTF(t *testing.T) (e *env, op *http.Client, opSecret string, logs *syncBuf) {
	t.Helper()
	logs = &syncBuf{}
	e = buildEnv(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}), openCfg, true, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	e.cfg.TrustProxy = true
	opSecret, _ = e.enable2FA(t, opEmail)
	return e, e.signInWithCode(t, "203.0.113.90", opEmail, opSecret), opSecret, logs
}

func reset2FAPath(id int64) string { return "/admin/users/" + strconv.FormatInt(id, 10) + "/reset-2fa" }

func flashAfter(t *testing.T, e *env, c *http.Client, resp *http.Response) string {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status %d, want 303", resp.StatusCode)
	}
	_, body := e.get(t, c, resp.Header.Get("Location"))
	return body
}

func TestAdminTwoFactorBadgeAndReset(t *testing.T) {
	e, op, _, logs := adminTF(t)
	e.account(t, "ann@example.com")
	e.account(t, "bob@example.com")
	annSecret, _ := e.enable2FA(t, "ann@example.com")
	ann := e.signInWithCode(t, "203.0.113.1", "ann@example.com", annSecret)
	pending := e.pendingSignIn(t, "203.0.113.2", "ann@example.com")
	annID, bobID := e.userID(t, "ann@example.com"), e.userID(t, "bob@example.com")

	_, body := e.get(t, op, "/admin/users")
	if strings.Count(body, `title="Two-factor authentication is on"`) != 2 { // ann and the operator
		t.Errorf("2FA badges = %d, want 2", strings.Count(body, `title="Two-factor authentication is on"`))
	}
	if !strings.Contains(body, reset2FAPath(annID)) || strings.Contains(body, reset2FAPath(bobID)) || strings.Contains(body, reset2FAPath(e.opID(t))) {
		t.Errorf("Reset 2FA offered for the wrong rows:\n%s", body)
	}
	if strings.Contains(body, msgReset2FANeed) {
		t.Error("the need-2FA note shown to an operator who has it")
	}

	resp, _ := e.post(t, op, reset2FAPath(annID), url.Values{})
	if m := flashAfter(t, e, op, resp); !strings.Contains(m, msgReset2FADone) {
		t.Fatalf("reset: %s", m)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), annID); enabled {
		t.Fatal("2FA still on")
	}
	if resp, _ := e.get(t, ann, "/settings/security"); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("target still signed in: %d", resp.StatusCode)
	}
	if _, err := e.st.PendingLogin(t.Context(), jarCookie(pending, "uptime_mfa").Value, e.now()); err == nil {
		t.Error("pending sign-in survived the reset")
	}
	if n := countMail(e, msgMFAResetSubject); n != 1 {
		t.Fatalf("notices = %d", n)
	}
	for _, m := range e.mail.all() {
		if strings.Contains(m.Subject, msgMFAResetSubject) && (m.To != "ann@example.com" || !strings.Contains(m.Body, "An administrator turned off two-factor authentication on your account.")) {
			t.Errorf("notice: %+v", m)
		}
	}
	if l := logs.String(); !strings.Contains(l, "event=mfa_reset") || !strings.Contains(l, "actor="+strconv.FormatInt(e.opID(t), 10)) || !strings.Contains(l, "user_id="+strconv.FormatInt(annID, 10)) {
		t.Errorf("no mfa_reset log with the actor:\n%s", l)
	}
	// Anything secret stays out of the log.
	if strings.Contains(logs.String(), annSecret) {
		t.Error("the secret is in the log")
	}
	// Ann signs in with the password alone now.
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {"ann@example.com"}, "password": {testSecret}}); resp.Header.Get("Location") != "/" {
		t.Errorf("sign-in after reset: %q", resp.Header.Get("Location"))
	}
}

func TestAdminTwoFactorResetRefusals(t *testing.T) {
	e, op, _, _ := adminTF(t)
	e.account(t, "bob@example.com")
	bobID := e.userID(t, "bob@example.com")
	// Operators, the acting one included.
	resp, _ := e.post(t, op, reset2FAPath(e.opID(t)), url.Values{})
	if m := flashAfter(t, e, op, resp); !strings.Contains(m, "Operators") {
		t.Errorf("operator target: %s", m)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), e.opID(t)); !enabled {
		t.Fatal("an operator's 2FA was reset from the panel")
	}
	// An account without two-factor, and no notice for it.
	resp, _ = e.post(t, op, reset2FAPath(bobID), url.Values{})
	if m := flashAfter(t, e, op, resp); !strings.Contains(m, "doesn&#39;t have two-factor") {
		t.Errorf("not on: %s", m)
	}
	if countMail(e, msgMFAResetSubject) != 0 {
		t.Error("a notice for an account without 2FA")
	}
	// Unknown id.
	if resp, _ = e.post(t, op, reset2FAPath(99999), url.Values{}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
	// Not an operator: 404, like the rest of the panel.
	c, _ := e.account(t, "carol@example.com")
	if resp, _ = e.post(t, c, reset2FAPath(bobID), url.Values{}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("non-operator: %d", resp.StatusCode)
	}
}

func TestAdminTwoFactorResetNeedsActorTwoFactor(t *testing.T) {
	e := newEnvMail(t, openCfg)
	e.account(t, "ann@example.com")
	e.enable2FA(t, "ann@example.com")
	annID := e.userID(t, "ann@example.com")
	_, body := e.get(t, e.op, "/admin/users")
	if !strings.Contains(body, msgReset2FANeed) || strings.Contains(body, reset2FAPath(annID)) {
		t.Fatalf("operator without 2FA: note %v, form %v", strings.Contains(body, msgReset2FANeed), strings.Contains(body, reset2FAPath(annID)))
	}
	resp, _ := e.post(t, e.op, reset2FAPath(annID), url.Values{})
	if m := flashAfter(t, e, e.op, resp); !strings.Contains(m, msgReset2FANeed) {
		t.Errorf("refusal: %s", m)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), annID); !enabled {
		t.Fatal("reset by an operator without two-factor")
	}
}

func TestAdminTwoFactorResetNeedsRecentCode(t *testing.T) {
	e, op, opSecret, _ := adminTF(t)
	e.account(t, "ann@example.com")
	e.enable2FA(t, "ann@example.com")
	annID := e.userID(t, "ann@example.com")
	e.advance(11 * time.Minute)
	_, body := e.get(t, op, "/admin/users")
	if !strings.Contains(body, reset2FAPath(annID)) || !strings.Contains(body, `name="code"`) {
		t.Fatalf("stale operator: no code field:\n%s", body)
	}
	// Without a code, with a wrong one: refused.
	if resp, _ := e.post(t, op, reset2FAPath(annID), url.Values{}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no code: %d", resp.StatusCode)
	}
	if resp, _ := e.post(t, op, reset2FAPath(annID), url.Values{"code": {"000000"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong code: %d", resp.StatusCode)
	}
	if _, enabled, _ := e.st.TOTPSecret(t.Context(), annID); !enabled {
		t.Fatal("reset without a recent code")
	}
	resp, _ := e.post(t, op, reset2FAPath(annID), url.Values{"code": {e.code(t, opSecret)}})
	if m := flashAfter(t, e, op, resp); !strings.Contains(m, msgReset2FADone) {
		t.Fatalf("with a code: %s", m)
	}
}

func TestAdminResetCSRF(t *testing.T) {
	e, op, _, _ := adminTF(t)
	req, _ := http.NewRequest("POST", e.ts.URL+reset2FAPath(2), strings.NewReader(""))
	req.Header.Set("Origin", "https://evil.example")
	resp, err := op.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site: %d", resp.StatusCode)
	}
}

var _ = store.ErrNotFound
