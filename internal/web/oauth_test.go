package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/oauth"
	"github.com/rahuldr07/uptime-monitor/internal/oauth/oauthtest"
	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// oenv is an env whose server offers the fake providers.
type oenv struct {
	*env
	g, gh *oauthtest.Fake // nil when not enabled
	logs  *syncBuf
}

// newOEnv builds a server with the named fake providers (both when none are named).
func newOEnv(t *testing.T, cfgJSON string, kinds ...string) *oenv {
	t.Helper()
	if len(kinds) == 0 {
		kinds = []string{"google", "github"}
	}
	o := &oenv{logs: &syncBuf{}}
	// Sign-up modes need a base URL, and open needs mail; BaseURL is reset to the test server below.
	const base = `{"base_url":"http://127.0.0.1","mail":{"smtp_host":"x.example","from":"up@example.com"}`
	if cfgJSON == "{}" {
		cfgJSON = base + "}"
	} else {
		cfgJSON = base + "," + strings.TrimPrefix(cfgJSON, "{")
	}
	var provs []*oauth.Provider
	for _, k := range kinds {
		f := oauthtest.New(t, k)
		if k == "google" {
			o.g = f
		} else {
			o.gh = f
		}
		provs = append(provs, f.Provider())
	}
	o.env = buildEnv(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}), cfgJSON, true, func(op *Options) {
		op.OAuth = provs
		op.Logger = slog.New(slog.NewTextHandler(o.logs, nil))
	})
	o.cfg.BaseURL = o.ts.URL // loopback http, as the redirect URI base
	return o
}

func (o *oenv) fake(provider string) *oauthtest.Fake {
	if provider == "google" {
		return o.g
	}
	return o.gh
}

func (o *oenv) fetch(t *testing.T, c *http.Client, full string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(full)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// start GETs /auth/<provider>/start?<query>.
func (o *oenv) start(t *testing.T, c *http.Client, provider, query string) *http.Response {
	t.Helper()
	resp, _ := o.fetch(t, c, o.ts.URL+"/auth/"+provider+"/start?"+query)
	return resp
}

// authorize visits the fake's authorize URL (no cookies) and returns the callback URL it bounces to.
func (o *oenv) authorize(t *testing.T, loc string) string {
	t.Helper()
	nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := nc.Get(loc)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

// begin starts a flow and returns the callback URL and the oauth cookie.
func (o *oenv) begin(t *testing.T, c *http.Client, provider, query string) (cb string, ck *http.Cookie) {
	t.Helper()
	resp := o.start(t, c, provider, query)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	return o.authorize(t, resp.Header.Get("Location")), cookieNamed(resp, "uptime_oauth")
}

// run drives a whole flow and returns the callback's response.
func (o *oenv) run(t *testing.T, c *http.Client, provider, query string) (*http.Response, string) {
	t.Helper()
	cb, _ := o.begin(t, c, provider, query)
	return o.fetch(t, c, cb)
}

// social creates a social-only account with its identity.
func (o *oenv) social(t *testing.T, email, provider, subject string) store.User {
	t.Helper()
	u, _, err := o.st.CreateAccount(t.Context(), store.AccountInput{Email: email, WorkspaceName: "My workspace",
		Verified: true, Identity: &store.NewIdentity{Provider: provider, Subject: subject, Email: email}, Now: o.now()})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (o *oenv) userID(t *testing.T, email string) int64 {
	t.Helper()
	u, _, err := o.st.UserByEmail(t.Context(), email)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func (o *oenv) noUser(t *testing.T, email string) {
	t.Helper()
	if _, _, err := o.st.UserByEmail(t.Context(), email); err == nil {
		t.Fatalf("account for %s exists", email)
	}
}

func jarCookie(c *http.Client, name string) *http.Cookie {
	for _, ck := range c.Jar.Cookies(nil) {
		if ck.Name == name {
			return ck
		}
	}
	return nil
}

func signedIn(t *testing.T, o *oenv, c *http.Client) bool {
	t.Helper()
	resp, _ := o.get(t, c, "/account")
	return resp.StatusCode == http.StatusOK
}

func wantFail(t *testing.T, resp *http.Response, body string, code int, msg string) {
	t.Helper()
	if resp.StatusCode != code || !strings.Contains(body, msg) {
		t.Fatalf("status %d (want %d), body should contain %q: %s", resp.StatusCode, code, msg, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
}

func flashOf(t *testing.T, resp *http.Response) (path, msg string) {
	t.Helper()
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u.Path, u.Query().Get("msg")
}

func TestOAuthStart(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	c := o.client(t)
	resp := o.start(t, c, "google", "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("start: %d cache %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if loc.Path != "/authorize" || q.Get("redirect_uri") != o.ts.URL+"/auth/google/callback" || q.Get("state") == "" ||
		q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "fake-client" {
		t.Fatalf("authorize URL: %s", loc)
	}
	ck := cookieNamed(resp, "uptime_oauth")
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.MaxAge != 600 || ck.Path != "/" || ck.Secure || ck.Value == "" {
		t.Fatalf("oauth cookie: %+v", ck)
	}

	// Not found: unknown provider, provider not enabled, bad intent, link and reauth without a session.
	for _, p := range []string{"/auth/nope/start", "/auth/github/start", "/auth/google/start?intent=bogus",
		"/auth/google/start?intent=link", "/auth/google/start?intent=reauth", "/auth/nope/callback", "/auth/github/callback"} {
		if r, _ := o.get(t, c, p); r.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, r.StatusCode)
		}
	}
	// POST is not routed.
	if r, _ := o.post(t, c, "/auth/google/start", nil); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST start: %d", r.StatusCode)
	}
	// Link and reauth need a session.
	alice, _ := o.account(t, "alice@example.com")
	for _, in := range []string{"link", "reauth"} {
		if r := o.start(t, alice, "google", "intent="+in); r.StatusCode != http.StatusSeeOther {
			t.Errorf("signed-in %s start: %d", in, r.StatusCode)
		}
	}
	// A signed-in user is not bounced from the public route.
	if r := o.start(t, alice, "google", ""); r.StatusCode != http.StatusSeeOther || !strings.Contains(r.Header.Get("Location"), "/authorize") {
		t.Errorf("signed-in login start: %d %s", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestOAuthCookieNameOverHTTPS(t *testing.T) {
	o := newOEnv(t, `{"trust_proxy":true}`, "google")
	req, _ := http.NewRequest("GET", o.ts.URL+"/auth/google/start", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ck := cookieNamed(resp, "__Host-uptime_oauth")
	if ck == nil || !ck.Secure || !ck.HttpOnly || ck.Path != "/" || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie: %+v (%v)", ck, resp.Cookies())
	}
}

// Decision table: login, identity linked.
func TestOAuthLoginLinked(t *testing.T) {
	o := newOEnv(t, `{}`)
	u := o.social(t, "person@example.com", "google", "12345")
	other, _ := o.account(t, "other@example.com")
	oldRaw := jarCookie(other, "uptime_session").Value
	cb, oc := o.begin(t, other, "google", "next=%2Ftargets")
	st := mustQuery(t, cb).Get("state")
	resp, _ := o.fetch(t, other, cb)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/targets" {
		t.Fatalf("callback: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if cookieNamed(resp, "uptime_session") == nil || cookieNamed(resp, "uptime_device") == nil {
		t.Errorf("session and device cookies expected: %v", resp.Cookies())
	}
	if ck := cookieNamed(resp, "uptime_oauth"); ck == nil || ck.MaxAge >= 0 {
		t.Errorf("oauth cookie not cleared: %+v", ck)
	}
	if _, err := o.st.SessionAuth(t.Context(), oldRaw, o.now()); err == nil {
		t.Error("previous session survived")
	}
	if !signedIn(t, o, other) {
		t.Error("not signed in")
	}
	ids, _ := o.st.Identities(t.Context(), u.ID)
	if len(ids) != 1 || ids[0].LastUsedAt == nil {
		t.Errorf("last_used_at not updated: %+v", ids)
	}
	logs := o.logs.String()
	if !strings.Contains(logs, "event=oauth_login_ok") {
		t.Errorf("no oauth_login_ok event:\n%s", logs)
	}
	for _, secret := range []string{st, oc.Value, "tok-code", oauthtest.Secret} {
		if strings.Contains(logs, secret) {
			t.Errorf("log leaks %q", secret)
		}
	}

	// GitHub works the same, by its own subject.
	o.gh.Subject = "777"
	o.social(t, "gh@example.com", "github", "777")
	o.gh.Email = "gh@example.com"
	c := o.client(t)
	if resp, _ := o.run(t, c, "github", ""); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || !signedIn(t, o, c) {
		t.Fatalf("github login: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestOAuthLoginNextIsSafe(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	for _, next := range []string{"//evil.com", "https://evil.com", "/\\evil.com", "/auth/google/start", "/auth/github/callback?x=1"} {
		c := o.client(t)
		resp, _ := o.run(t, c, "google", "next="+url.QueryEscape(next))
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
			t.Errorf("next %q: %d %q", next, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

func TestOAuthLoginDisabled(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	u := o.social(t, "person@example.com", "google", "12345")
	if _, err := o.st.SetUserDisabled(t.Context(), u.ID, true, o.now()); err != nil {
		t.Fatal(err)
	}
	c := o.client(t)
	resp, body := o.run(t, c, "google", "")
	wantFail(t, resp, body, http.StatusForbidden, msgDisabled)
	if cookieNamed(resp, "uptime_session") != nil || signedIn(t, o, c) {
		t.Error("disabled user got a session")
	}
}

// Decision table: login, not linked, email owned by an account.
func TestOAuthLoginUnlinkedEmailOwned(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	_, _ = o.account(t, "person@example.com")
	uid := o.userID(t, "person@example.com")
	c := o.client(t)
	resp, body := o.run(t, c, "google", "")
	want := "An account with this email already exists. Sign in with your password (or use Forgot password if you don't have one), then connect Google from your Account page."
	wantFail(t, resp, body, http.StatusConflict, strings.ReplaceAll(want, "'", "&#39;"))
	if ids, _ := o.st.Identities(t.Context(), uid); len(ids) != 0 {
		t.Errorf("identity linked: %+v", ids)
	}
	if cookieNamed(resp, "uptime_session") != nil || signedIn(t, o, c) {
		t.Error("unlinked sign-in created a session")
	}
	logs := o.logs.String()
	if !strings.Contains(logs, "event=oauth_login_unlinked") || !strings.Contains(logs, "email_hash="+emailHash("person@example.com")) ||
		strings.Contains(logs, "person@example.com") {
		t.Errorf("log:\n%s", logs)
	}

	// An unverified account is the same, and so is closed mode: no sign-up is attempted.
	if _, _, err := o.st.CreateAccount(t.Context(), store.AccountInput{Email: "unv@example.com", PasswordHash: "x",
		WorkspaceName: "U", Slug: "u", Now: o.now()}); err != nil {
		t.Fatal(err)
	}
	o.g.Email = "unv@example.com"
	resp, body = o.run(t, o.client(t), "google", "")
	wantFail(t, resp, body, http.StatusConflict, "already exists")
}

// Decision table: login, not linked, no account: sign-up.
func TestOAuthSignupOpen(t *testing.T) {
	o := newOEnv(t, `{"signup":"open"}`, "google")
	c := o.client(t)
	resp, _ := o.run(t, c, "google", "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || !signedIn(t, o, c) {
		t.Fatalf("sign-up: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	u, phc, err := o.st.UserByEmail(t.Context(), "person@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if phc != "" || u.VerifiedAt == nil {
		t.Errorf("hash %q verified %v", phc, u.VerifiedAt)
	}
	au, err := o.st.SessionAuth(t.Context(), jarCookie(c, "uptime_session").Value, o.now())
	if err != nil {
		t.Fatal(err)
	}
	if au.Workspace.Name != "My workspace" || strings.Contains(au.Workspace.Slug, "person") || au.HasPassword {
		t.Errorf("workspace %q slug %q hasPassword %v", au.Workspace.Name, au.Workspace.Slug, au.HasPassword)
	}
	ids, _ := o.st.Identities(t.Context(), u.ID)
	if len(ids) != 1 || ids[0].Provider != "google" || ids[0].Subject != "12345" || ids[0].Email != "person@example.com" {
		t.Errorf("identities: %+v", ids)
	}
	if !strings.Contains(o.logs.String(), "event=oauth_signup") {
		t.Errorf("no oauth_signup event:\n%s", o.logs.String())
	}
	// And the same identity signs straight in next time.
	c2 := o.client(t)
	if resp, _ := o.run(t, c2, "google", ""); resp.StatusCode != http.StatusSeeOther || !signedIn(t, o, c2) {
		t.Fatalf("second sign-in: %d", resp.StatusCode)
	}
}

func TestOAuthSignupClosedAndInviteModes(t *testing.T) {
	for _, mode := range []string{"closed", "invite"} {
		o := newOEnv(t, `{"signup":"`+mode+`"}`, "google")
		c := o.client(t)
		resp, body := o.run(t, c, "google", "")
		wantFail(t, resp, body, http.StatusForbidden, "Sign-up on this instance is by invitation.")
		o.noUser(t, "person@example.com")

		// A bogus invite is refused too.
		resp, body = o.run(t, o.client(t), "google", "invite=bogus")
		wantFail(t, resp, body, http.StatusForbidden, msgBadInvite)
		o.noUser(t, "person@example.com")

		// A valid invite signs up, and is consumed.
		raw := o.invite(t, "")
		c = o.client(t)
		resp, _ = o.run(t, c, "google", "invite="+raw)
		if resp.StatusCode != http.StatusSeeOther || !signedIn(t, o, c) {
			t.Fatalf("%s mode with invite: %d", mode, resp.StatusCode)
		}
		if _, err := o.st.PeekToken(t.Context(), store.TokenInvite, raw, o.now()); err == nil {
			t.Error("invite not consumed")
		}
		o.g.Subject, o.g.Email = "2", "second@example.com"
		resp, body = o.run(t, o.client(t), "google", "invite="+raw)
		wantFail(t, resp, body, http.StatusForbidden, msgBadInvite)
		o.noUser(t, "second@example.com")
	}
}

func TestOAuthSignupInviteBoundToEmail(t *testing.T) {
	o := newOEnv(t, `{"signup":"invite"}`, "google")
	raw := o.invite(t, "someone-else@example.com")
	resp, body := o.run(t, o.client(t), "google", "invite="+raw)
	wantFail(t, resp, body, http.StatusForbidden, msgInviteEmail)
	o.noUser(t, "person@example.com")
	if _, err := o.st.PeekToken(t.Context(), store.TokenInvite, raw, o.now()); err != nil {
		t.Errorf("invite spent by a refused sign-up: %v", err)
	}
	raw = o.invite(t, "person@example.com")
	c := o.client(t)
	if resp, _ := o.run(t, c, "google", "invite="+raw); resp.StatusCode != http.StatusSeeOther || !signedIn(t, o, c) {
		t.Fatalf("matching invite: %d", resp.StatusCode)
	}
}

func TestOAuthSignupWorkspaceCap(t *testing.T) {
	o := newOEnv(t, `{"signup":"open","limits":{"max_workspaces":1}}`, "google")
	resp, body := o.run(t, o.client(t), "google", "")
	wantFail(t, resp, body, http.StatusForbidden, msgNoNewAccounts)
	o.noUser(t, "person@example.com")
}

func TestOAuthSignupLimits(t *testing.T) {
	o := newOEnv(t, `{"signup":"open"}`, "google")
	for i := 0; i < signupPerAddrHour; i++ {
		o.g.Subject, o.g.Email = fmt.Sprint(100+i), fmt.Sprintf("u%d@example.com", i)
		if resp, _ := o.run(t, o.client(t), "google", ""); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("sign-up %d: %d", i, resp.StatusCode)
		}
	}
	o.g.Subject, o.g.Email = "200", "late@example.com"
	resp, body := o.run(t, o.client(t), "google", "")
	wantFail(t, resp, body, http.StatusTooManyRequests, msgTooMany)
	o.noUser(t, "late@example.com")
	// Existing identities still sign in: the limit is on sign-ups.
	o.g.Subject, o.g.Email = "100", "u0@example.com"
	if resp, _ := o.run(t, o.client(t), "google", ""); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("login while sign-ups are limited: %d", resp.StatusCode)
	}

	g := newOEnv(t, `{"signup":"open","limits":{"signups_per_hour":1}}`, "google")
	if resp, _ := g.run(t, g.client(t), "google", ""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	g.g.Subject, g.g.Email = "2", "two@example.com"
	resp, body = g.run(t, g.client(t), "google", "")
	wantFail(t, resp, body, http.StatusTooManyRequests, msgTooMany)
}

// Decision table: link rows.
func TestOAuthLink(t *testing.T) {
	o := newOEnv(t, `{}`)
	alice, _ := o.account(t, "alice@example.com")
	aliceID := o.userID(t, "alice@example.com")

	// no identity: link; the provider email may differ from the account email.
	resp, _ := o.run(t, alice, "google", "intent=link")
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/account" || m != "Google connected." {
		t.Fatalf("link: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	ids, _ := o.st.Identities(t.Context(), aliceID)
	if len(ids) != 1 || ids[0].Subject != "12345" || ids[0].Email != "person@example.com" {
		t.Fatalf("identities: %+v", ids)
	}
	if !strings.Contains(o.logs.String(), "event=oauth_link") {
		t.Error("no oauth_link event")
	}
	// the flash is signed, so /account shows it
	if _, body := o.get(t, alice, resp.Header.Get("Location")); !strings.Contains(body, "Google connected.") {
		t.Error("flash not shown on /account")
	}

	// already linked to this user
	resp, _ = o.run(t, alice, "google", "intent=link")
	if p, m := flashOf(t, resp); resp.StatusCode != http.StatusSeeOther || p != "/account" || m != "Google is already connected." {
		t.Errorf("relink: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// the user already has a different Google account linked
	o.g.Subject = "999"
	resp, body := o.run(t, alice, "google", "intent=link")
	wantFail(t, resp, body, http.StatusConflict, "You already have a Google account connected")

	// linked to another user
	bob := o.social(t, "bob@example.com", "github", "55")
	o.gh.Subject = "55"
	resp, body = o.run(t, alice, "github", "intent=link")
	wantFail(t, resp, body, http.StatusConflict, "This GitHub account is already connected to a different account.")
	if ids, _ := o.st.Identities(t.Context(), bob.ID); len(ids) != 1 {
		t.Errorf("bob's identities changed: %+v", ids)
	}
	if ids, _ := o.st.Identities(t.Context(), aliceID); len(ids) != 1 {
		t.Errorf("alice's identities changed: %+v", ids)
	}
}

func TestOAuthLinkCookieForAnotherUser(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	alice, _ := o.account(t, "alice@example.com")
	bob, _ := o.account(t, "bob@example.com")
	cb, ck := o.begin(t, alice, "google", "intent=link")
	bob.Jar.SetCookies(nil, []*http.Cookie{ck})
	resp, body := o.fetch(t, bob, cb)
	wantFail(t, resp, body, http.StatusBadRequest, msgOAuthFailed)
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		if ids, _ := o.st.Identities(t.Context(), o.userID(t, email)); len(ids) != 0 {
			t.Errorf("%s got an identity", email)
		}
	}
	// A link flow finished without any session at all is refused too.
	cb, ck = o.begin(t, alice, "google", "intent=link")
	anon := o.client(t)
	anon.Jar.SetCookies(nil, []*http.Cookie{ck})
	resp, body = o.fetch(t, anon, cb)
	wantFail(t, resp, body, http.StatusBadRequest, msgOAuthFailed)
}

// Decision table: reauth rows.
func TestOAuthReauth(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	c := o.client(t)
	if resp, _ := o.run(t, c, "google", ""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %d", resp.StatusCode)
	}
	raw := jarCookie(c, "uptime_session").Value
	other := o.client(t) // a second session of the same user
	if r, _ := o.run(t, other, "google", ""); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("second sign-in: %d", r.StatusCode)
	}
	otherRaw := jarCookie(other, "uptime_session").Value
	signedAt := o.now()
	o.advance(5 * time.Minute)
	resp, _ := o.run(t, c, "google", "intent=reauth")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/account" {
		t.Fatalf("reauth: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	au, err := o.st.SessionAuth(t.Context(), raw, o.now())
	if err != nil {
		t.Fatal(err)
	}
	if au.AuthAt == nil || !au.AuthAt.Equal(o.now()) {
		t.Errorf("auth_at = %v, want %v", au.AuthAt, o.now())
	}
	if oa, err := o.st.SessionAuth(t.Context(), otherRaw, o.now()); err != nil || oa.AuthAt == nil || !oa.AuthAt.Equal(signedAt) {
		t.Errorf("another session's auth_at = %v (%v), want %v", oa.AuthAt, err, signedAt)
	}
	if !strings.Contains(o.logs.String(), "event=oauth_reauth") {
		t.Error("no oauth_reauth event")
	}
	resp, _ = o.run(t, c, "google", "intent=reauth&next=%2Ftargets")
	if resp.Header.Get("Location") != "/targets" {
		t.Errorf("reauth next: %s", resp.Header.Get("Location"))
	}

	// A Google account that is not the one connected to this account.
	o.g.Subject = "999"
	resp, body := o.run(t, c, "google", "intent=reauth")
	wantFail(t, resp, body, http.StatusBadRequest, "Sign in with the Google account connected to this account.")
	// A user with no Google identity at all.
	alice, _ := o.account(t, "alice@example.com")
	resp, body = o.run(t, alice, "google", "intent=reauth")
	wantFail(t, resp, body, http.StatusBadRequest, "Sign in with the Google account connected to this account.")
	// An identity that belongs to another user.
	o.g.Subject = "12345"
	resp, body = o.run(t, alice, "google", "intent=reauth")
	wantFail(t, resp, body, http.StatusBadRequest, "Sign in with the Google account connected to this account.")
}

func TestOAuthCallbackRejections(t *testing.T) {
	type tc struct {
		name string
		do   func(o *oenv, c *http.Client) (*http.Response, string)
	}
	cases := []tc{
		{"no cookie", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, o.client(t), "google", "")
			return o.fetch(t, c, cb)
		}},
		{"bad signature", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			ck := jarCookie(c, "uptime_oauth")
			p, sig, _ := strings.Cut(ck.Value, ".")
			mac, _ := base64.RawURLEncoding.DecodeString(sig)
			mac[0] ^= 1 // flip one decoded MAC bit
			ck.Value = p + "." + base64.RawURLEncoding.EncodeToString(mac)
			return o.fetch(t, c, cb)
		}},
		{"re-encoded payload, old signature", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			ck := jarCookie(c, "uptime_oauth")
			p, sig, _ := strings.Cut(ck.Value, ".")
			raw, _ := base64.RawURLEncoding.DecodeString(p)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			m["u"] = 1 // pretend the flow belongs to another user
			m["i"] = "link"
			raw, _ = json.Marshal(m)
			ck.Value = base64.RawURLEncoding.EncodeToString(raw) + "." + sig
			return o.fetch(t, c, cb)
		}},
		{"tampered payload", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			ck := jarCookie(c, "uptime_oauth")
			ck.Value = "x" + ck.Value
			return o.fetch(t, c, cb)
		}},
		{"garbage cookie", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			jarCookie(c, "uptime_oauth").Value = "not.a.cookie"
			return o.fetch(t, c, cb)
		}},
		{"expired", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			o.advance(oauthTTL + time.Second)
			return o.fetch(t, c, cb)
		}},
		{"wrong provider in cookie", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			return o.fetch(t, c, strings.Replace(cb, "/auth/google/", "/auth/github/", 1))
		}},
		{"state mismatch", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			q := mustQuery(t, cb)
			q.Set("state", q.Get("state")+"x")
			return o.fetch(t, c, o.ts.URL+"/auth/google/callback?"+q.Encode())
		}},
		{"no state", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			q := mustQuery(t, cb)
			q.Del("state")
			return o.fetch(t, c, o.ts.URL+"/auth/google/callback?"+q.Encode())
		}},
		{"provider error", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			q := mustQuery(t, cb)
			return o.fetch(t, c, o.ts.URL+"/auth/google/callback?error=access_denied&state="+url.QueryEscape(q.Get("state")))
		}},
		{"no code", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			q := mustQuery(t, cb)
			return o.fetch(t, c, o.ts.URL+"/auth/google/callback?state="+url.QueryEscape(q.Get("state")))
		}},
		{"bad code", func(o *oenv, c *http.Client) (*http.Response, string) {
			cb, _ := o.begin(t, c, "google", "")
			q := mustQuery(t, cb)
			q.Set("code", "forged")
			return o.fetch(t, c, o.ts.URL+"/auth/google/callback?"+q.Encode())
		}},
		{"token endpoint 500", func(o *oenv, c *http.Client) (*http.Response, string) {
			o.g.FailToken = true
			return o.run(t, c, "google", "")
		}},
		{"error in a 200 token body", func(o *oenv, c *http.Client) (*http.Response, string) {
			o.g.TokenErrorIn200 = true
			return o.run(t, c, "google", "")
		}},
		{"token timeout", func(o *oenv, c *http.Client) (*http.Response, string) {
			old := oauthTimeout
			oauthTimeout = 100 * time.Millisecond
			t.Cleanup(func() { oauthTimeout = old })
			o.g.Delay = 600 * time.Millisecond
			return o.run(t, c, "google", "")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := newOEnv(t, `{"signup":"open"}`)
			cl := o.client(t)
			resp, body := c.do(o, cl)
			wantFail(t, resp, body, http.StatusBadRequest, msgOAuthFailed)
			if ck := cookieNamed(resp, "uptime_oauth"); ck == nil || ck.MaxAge >= 0 {
				t.Errorf("oauth cookie not cleared: %+v", ck)
			}
			if cookieNamed(resp, "uptime_session") != nil || signedIn(t, o, cl) {
				t.Error("session created")
			}
			o.noUser(t, "person@example.com")
			for _, leak := range []string{"server_error", "bad_verification_code", "invalid_grant", "access_denied", "nope"} {
				if strings.Contains(body, leak) {
					t.Errorf("provider text %q shown", leak)
				}
			}
			if !strings.Contains(o.logs.String(), "event=oauth_failed") {
				t.Errorf("no oauth_failed event:\n%s", o.logs.String())
			}
		})
	}
}

func TestOAuthNoVerifiedEmail(t *testing.T) {
	o := newOEnv(t, `{"signup":"open"}`)
	o.g.EmailVerified = false
	resp, body := o.run(t, o.client(t), "google", "")
	wantFail(t, resp, body, http.StatusBadRequest, "Your Google account has no verified email address.")
	o.gh.GitHubEmails = []oauthtest.GitHubEmail{{Email: "x@example.com", Primary: false, Verified: true}, {Email: "y@example.com", Primary: true, Verified: false}}
	resp, body = o.run(t, o.client(t), "github", "")
	wantFail(t, resp, body, http.StatusBadRequest, "Your GitHub account has no verified email address.")
	o.noUser(t, "person@example.com")
	o.noUser(t, "x@example.com")
}

// A copied cookie replayed with the same, already spent, code: the server
// refuses the state; TestOAuthStateUsedOnce covers a fresh code.
func TestOAuthReplayedCookie(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	cb, ck := o.begin(t, o.client(t), "google", "")
	if resp, _ := o.fetch(t, o.client(t), cb); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("callback without the cookie: %d", resp.StatusCode)
	}
	c := o.client(t)
	c.Jar.SetCookies(nil, []*http.Cookie{ck})
	if resp, _ := o.fetch(t, c, cb); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use: %d", resp.StatusCode)
	}
	// The browser dropped the cookie; a copy of it with the same code gets nowhere.
	// (The provider also refuses a reused code.)
	c2 := o.client(t)
	c2.Jar.SetCookies(nil, []*http.Cookie{ck})
	resp, body := o.fetch(t, c2, cb)
	wantFail(t, resp, body, http.StatusBadRequest, msgOAuthFailed)
	if signedIn(t, o, c2) {
		t.Error("replay signed in")
	}
}

// The state is spent by its first callback: a copied cookie with a fresh,
// valid code for the same authorization request is refused by the server.
func TestOAuthStateUsedOnce(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	start := o.start(t, o.client(t), "google", "")
	ck := cookieNamed(start, "uptime_oauth")
	loc := start.Header.Get("Location")
	cb1, cb2 := o.authorize(t, loc), o.authorize(t, loc)
	c1, c2 := o.client(t), o.client(t)
	c1.Jar.SetCookies(nil, []*http.Cookie{ck})
	c2.Jar.SetCookies(nil, []*http.Cookie{ck})
	if resp, _ := o.fetch(t, c1, cb1); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	resp, body := o.fetch(t, c2, cb2)
	wantFail(t, resp, body, http.StatusBadRequest, msgOAuthFailed)
	if signedIn(t, o, c2) || !strings.Contains(o.logs.String(), "reason=state_reused") {
		t.Error("reused state was accepted")
	}
}

func TestOAuthUnverifiedAccountRefused(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	if _, _, err := o.st.CreateAccount(t.Context(), store.AccountInput{Email: "person@example.com", WorkspaceName: "W",
		Identity: &store.NewIdentity{Provider: "google", Subject: "12345", Email: "person@example.com"}, Now: o.now()}); err != nil {
		t.Fatal(err)
	}
	c := o.client(t)
	resp, body := o.run(t, c, "google", "")
	wantFail(t, resp, body, http.StatusBadRequest, msgOAuthFailed)
	if cookieNamed(resp, "uptime_session") != nil || signedIn(t, o, c) {
		t.Error("unverified user got a session")
	}
}

// A refused sign-up gives its per-address slot back.
func TestOAuthSignupRefundsAddressSlot(t *testing.T) {
	o := newOEnv(t, `{"signup":"open","limits":{"signups_per_hour":1}}`, "google")
	if resp, _ := o.run(t, o.client(t), "google", ""); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	for i := 0; i < 3; i++ {
		o.g.Subject, o.g.Email = fmt.Sprint(50+i), fmt.Sprintf("r%d@example.com", i)
		if resp, _ := o.run(t, o.client(t), "google", ""); resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("refused %d: %d", i, resp.StatusCode)
		}
	}
	left := 0
	for {
		if ok, _ := o.srv.signupByAddr.Take("127.0.0.1"); !ok {
			break
		}
		left++
	}
	if left != signupPerAddrHour-1 {
		t.Errorf("address slots left = %d, want %d", left, signupPerAddrHour-1)
	}
}

// The limiter is keyed by the forwarded client address when a proxy is trusted.
func TestOAuthRateLimitPerForwardedAddress(t *testing.T) {
	o := newOEnv(t, `{"trust_proxy":true,"trusted_proxies":1}`, "google")
	nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(ip string) int {
		req, _ := http.NewRequest("GET", o.ts.URL+"/auth/google/start", nil)
		req.Header.Set("X-Forwarded-For", ip)
		resp, err := nc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < oauthPerMinute; i++ {
		if c := get("203.0.113.7"); c != http.StatusSeeOther {
			t.Fatalf("start %d: %d", i, c)
		}
	}
	if c := get("203.0.113.7"); c != http.StatusTooManyRequests {
		t.Errorf("over the limit: %d", c)
	}
	if c := get("203.0.113.8"); c != http.StatusSeeOther {
		t.Errorf("another client: %d", c)
	}
}

func TestOAuthRateLimit(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	c := o.client(t)
	cb, _ := o.begin(t, c, "google", "")                                      // 1
	if resp, _ := o.fetch(t, c, cb); resp.StatusCode != http.StatusSeeOther { // 2
		t.Fatalf("callback: %d", resp.StatusCode)
	}
	for i := 3; i <= oauthPerMinute; i++ {
		if resp := o.start(t, c, "google", ""); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("start %d: %d", i, resp.StatusCode)
		}
	}
	resp, body := o.fetch(t, c, o.ts.URL+"/auth/google/start")
	wantFail(t, resp, body, http.StatusTooManyRequests, msgTooMany)
	// callbacks draw on the same bucket
	resp, body = o.fetch(t, c, cb)
	wantFail(t, resp, body, http.StatusTooManyRequests, msgTooMany)
	o.advance(time.Minute + time.Second)
	if resp := o.start(t, c, "google", ""); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("after the window: %d", resp.StatusCode)
	}
}

// A social-only account has an empty hash: no password works and nothing returns 500.
func TestOAuthSocialOnlyAccount(t *testing.T) {
	o := newOEnv(t, `{}`, "google")
	o.social(t, "person@example.com", "google", "12345")
	c := o.client(t)
	var bodies []string
	for _, pw := range []string{"", "whatever", testSecret, strings.Repeat("x", 300)} {
		resp, body := o.post(t, c, "/login", url.Values{"email": {"person@example.com"}, "password": {pw}})
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, msgBadLogin) {
			t.Fatalf("password %q: %d %s", pw, resp.StatusCode, body)
		}
		bodies = append(bodies, body)
	}
	resp, unknown := o.post(t, c, "/login", url.Values{"email": {"nobody@example.com"}, "password": {"whatever"}})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(unknown, msgBadLogin) {
		t.Fatalf("unknown: %d", resp.StatusCode)
	}
	// Same page as for an unknown email, bar the echoed address.
	for _, b := range bodies {
		if strings.ReplaceAll(b, "person@example.com", "nobody@example.com") != unknown {
			t.Errorf("social-only response differs from the unknown-email response:\n%s\n---\n%s", b, unknown)
		}
	}

	// checkCurrent never verifies the empty hash.
	if r, _ := o.run(t, c, "google", ""); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in: %d", r.StatusCode)
	}
	for _, p := range []struct {
		path string
		form url.Values
	}{
		{"/account/delete", url.Values{"current": {"x"}}},
		{"/account/delete", url.Values{"current": {""}}},
		{"/account/password", url.Values{"current": {""}, "new": {testSecret}, "confirm": {testSecret}}},
	} {
		resp, body := o.post(t, c, p.path, p.form)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "Sign in again to continue") {
			t.Errorf("POST %s: %d %s", p.path, resp.StatusCode, body)
		}
	}
	if _, _, err := o.st.UserByEmail(t.Context(), "person@example.com"); err != nil {
		t.Errorf("account gone: %v", err)
	}
	if h, _ := o.st.PasswordHash(t.Context(), o.userID(t, "person@example.com")); h != "" {
		t.Error("password was set")
	}
}

func TestOAuthButtons(t *testing.T) {
	has := func(body, p string) bool { return strings.Contains(body, `href="/auth/`+p+`/start?`) }

	// no providers: nothing renders
	e := newEnv(t)
	c := e.client(t)
	_, body := e.get(t, c, "/login")
	if strings.Contains(body, "Continue with") || strings.Contains(body, "/auth/") {
		t.Errorf("buttons without providers: %s", body)
	}

	o := newOEnv(t, `{"signup":"open"}`)
	c = o.client(t)
	_, body = o.get(t, c, "/login?next=%2Ftargets")
	if !has(body, "google") || !has(body, "github") || !strings.Contains(body, "Continue with Google") ||
		!strings.Contains(body, "Continue with GitHub") || !strings.Contains(body, "next=%2Ftargets") {
		t.Errorf("login buttons: %s", body)
	}
	_, body = o.get(t, c, "/signup")
	if !has(body, "google") || !has(body, "github") || !strings.Contains(body, "Continue with Google") {
		t.Errorf("open sign-up buttons: %s", body)
	}
	if _, body = o.get(t, c, "/login"); strings.Contains(body, "next=") {
		t.Errorf("default next carried: %s", body)
	}

	// one provider only
	g := newOEnv(t, `{}`, "google")
	_, body = g.get(t, g.client(t), "/login")
	if !has(body, "google") || has(body, "github") || strings.Contains(body, "Continue with GitHub") {
		t.Errorf("google only: %s", body)
	}

	// invite mode and closed mode
	for _, mode := range []string{"invite", "closed"} {
		m := newOEnv(t, `{"signup":"`+mode+`"}`, "google")
		mc := m.client(t)
		if mode == "invite" {
			if _, body := m.get(t, mc, "/signup"); strings.Contains(body, "Continue with") {
				t.Errorf("invite mode, no invite: %s", body)
			}
		}
		if r, _ := m.get(t, mc, "/signup"); mode == "closed" && r.StatusCode != http.StatusNotFound {
			t.Errorf("closed /signup: %d", r.StatusCode)
		}
		raw := m.invite(t, "")
		_, body := m.get(t, mc, "/signup?invite="+raw)
		if !has(body, "google") || !strings.Contains(body, "invite="+raw) {
			t.Errorf("%s mode with invite: %s", mode, body)
		}
		if _, body := m.get(t, mc, "/signup?invite=bogus"); strings.Contains(body, "Continue with") {
			t.Errorf("%s mode, bad invite: %s", mode, body)
		}
		// the sign-in page offers it whatever the sign-up mode
		if _, body := m.get(t, mc, "/login"); !has(body, "google") {
			t.Errorf("%s mode login: %s", mode, body)
		}
	}
}
