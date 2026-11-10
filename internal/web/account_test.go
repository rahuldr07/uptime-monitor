package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"uptimemon/internal/store"
)

const newSecretPW = "another long passphrase"

func (e *env) opID(t *testing.T) int64 {
	t.Helper()
	u, _, err := e.st.UserByEmail(context.Background(), opEmail)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func (e *env) status(t *testing.T, c *http.Client) int {
	t.Helper()
	resp, _ := e.get(t, c, "/api/status")
	return resp.StatusCode
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	other := e.signIn(t, opEmail, testSecret)
	resp, body := e.post(t, e.op, "/account/password", url.Values{"current": {"wrong wrong wrong"}, "new": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Current password is incorrect.") {
		t.Fatalf("wrong current: %d %s", resp.StatusCode, body)
	}
	resp, _ = e.post(t, e.op, "/account/password", url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {"different different"}})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mismatch: %d", resp.StatusCode)
	}
	if e.status(t, other) != http.StatusOK {
		t.Fatal("failed attempts must not revoke sessions")
	}
	resp, _ = e.post(t, e.op, "/account/password", url.Values{"current": {testSecret}, "new": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/account?") {
		t.Fatalf("success: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if e.status(t, other) != http.StatusUnauthorized {
		t.Error("other session survived")
	}
	if e.status(t, e.op) != http.StatusOK {
		t.Error("current session was revoked")
	}
	_, body = e.get(t, e.op, resp.Header.Get("Location"))
	if !strings.Contains(body, "Password changed. Other sessions were signed out.") {
		t.Errorf("flash missing: %s", body)
	}
	c := e.client(t)
	if resp, _ := e.post(t, c, "/login", url.Values{"email": {opEmail}, "password": {newSecretPW}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("new password sign-in: %d", resp.StatusCode)
	}
	c = e.client(t)
	if resp, _ := e.post(t, c, "/login", url.Values{"email": {opEmail}, "password": {testSecret}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old password sign-in: %d", resp.StatusCode)
	}
}

func TestSignOutEverywhere(t *testing.T) {
	e := newEnv(t)
	other := e.signIn(t, opEmail, testSecret)
	resp, _ := e.post(t, e.op, "/account/signout-all", url.Values{})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?") {
		t.Fatalf("signout-all: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	for name, c := range map[string]*http.Client{"this": e.op, "other": other} {
		if got := e.status(t, c); got != http.StatusUnauthorized {
			t.Errorf("%s client: %d, want 401", name, got)
		}
	}
	_, body := e.get(t, e.client(t), resp.Header.Get("Location"))
	if !strings.Contains(body, "Signed out everywhere.") {
		t.Errorf("login page lacks flash: %s", body)
	}
}

func (e *env) resetToken(t *testing.T) string {
	t.Helper()
	tok, err := e.st.CreateToken(context.Background(), store.TokenReset, e.opID(t), time.Hour, e.clock)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestResetFlow(t *testing.T) {
	e := newEnv(t)
	tok := e.resetToken(t)
	c := e.client(t)
	resp, body := e.get(t, c, "/reset?token="+tok)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="password"`) || !strings.Contains(body, tok) {
		t.Fatalf("GET: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", resp.Header.Get("Cache-Control"))
	}
	if _, err := e.st.PeekToken(context.Background(), store.TokenReset, tok, e.clock); err != nil {
		t.Fatalf("GET consumed the token: %v", err)
	}
	resp, body = e.post(t, c, "/reset", url.Values{"token": {tok}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("POST: %d %q %s", resp.StatusCode, resp.Header.Get("Location"), body)
	}
	if e.status(t, e.op) != http.StatusUnauthorized {
		t.Error("prior session survived")
	}
	if e.status(t, c) != http.StatusOK {
		t.Error("new session does not work")
	}
	if resp, _ := e.post(t, e.client(t), "/login", url.Values{"email": {opEmail}, "password": {testSecret}}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old password: %d", resp.StatusCode)
	}
	resp, _ = e.post(t, e.client(t), "/reset", url.Values{"token": {tok}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("reused token: %d, want 404", resp.StatusCode)
	}
}

func TestResetGetNeverConsumes(t *testing.T) {
	e := newEnv(t)
	tok := e.resetToken(t)
	c := e.client(t)
	for i := 0; i < 2; i++ {
		if resp, _ := e.get(t, c, "/reset?token="+tok); resp.StatusCode != 200 {
			t.Fatalf("GET %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := e.post(t, c, "/reset", url.Values{"token": {tok}, "password": {newSecretPW}, "confirm": {newSecretPW}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST after GETs: %d", resp.StatusCode)
	}
	_, body := e.get(t, e.client(t), "/reset?token=bogus")
	if !strings.Contains(body, "This link is invalid or has expired.") || strings.Contains(body, `name="password"`) {
		t.Errorf("bogus token page: %s", body)
	}
}
