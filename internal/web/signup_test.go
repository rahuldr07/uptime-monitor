package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

func newSignupEnv(t *testing.T, cfgJSON string) *env {
	t.Helper()
	return newEnvConfig(t, password.New(password.Params{Memory: 64, Time: 1, Threads: 1}), cfgJSON)
}

func (e *env) invite(t *testing.T, email string) string {
	t.Helper()
	raw, err := e.st.CreateInvite(t.Context(), email, time.Hour, e.now())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func signupForm(invite, email string) url.Values {
	return url.Values{"invite": {invite}, "email": {email}, "password": {testSecret},
		"confirm": {testSecret}, "workspace": {"Acme Team"}}
}

func TestSignupModes(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"closed"}`)
	c := e.client(t)
	if resp, _ := e.get(t, c, "/signup"); resp.StatusCode != 404 {
		t.Fatalf("closed, no invite: %d", resp.StatusCode)
	}
	raw := e.invite(t, "")
	resp, body := e.get(t, c, "/signup?invite="+raw)
	if resp.StatusCode != 200 || !strings.Contains(body, `name="workspace"`) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("closed, valid invite: %d %s", resp.StatusCode, body)
	}
	if _, body := e.get(t, c, "/login"); strings.Contains(body, "invitation link") {
		t.Fatal("closed mode should not hint at sign-up")
	}

	e.cfg.Signup = "invite"
	resp, body = e.get(t, c, "/signup")
	if resp.StatusCode != 200 || !strings.Contains(body, msgInviteOnly) || strings.Contains(body, `name="workspace"`) {
		t.Fatalf("invite, no invite: %d %s", resp.StatusCode, body)
	}
	if _, body := e.get(t, c, "/signup?invite="+raw); !strings.Contains(body, `name="workspace"`) {
		t.Fatalf("invite, valid: %s", body)
	}
	resp, body = e.get(t, c, "/signup?invite=bogus")
	if resp.StatusCode != 200 || !strings.Contains(body, msgBadInvite) || strings.Contains(body, `name="workspace"`) {
		t.Fatalf("invalid invite: %d %s", resp.StatusCode, body)
	}
	if _, body := e.get(t, c, "/login"); !strings.Contains(body, "Have an invitation link? Open it to sign up.") {
		t.Fatal("invite mode should hint at sign-up on /login")
	}
	if got := safeNext("/signup?invite=x"); got != "/" {
		t.Fatalf("safeNext allows /signup: %q", got)
	}
}

func TestInviteBoundEmailPrefilled(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	raw := e.invite(t, "bound@example.com")
	_, body := e.get(t, e.client(t), "/signup?invite="+raw)
	if !strings.Contains(body, `value="bound@example.com"`) || !strings.Contains(body, "readonly") {
		t.Fatalf("bound email not prefilled read-only: %s", body)
	}
}

func TestInviteSignupFlow(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	raw := e.invite(t, "")
	c := e.client(t)
	resp, body := e.post(t, c, "/signup", signupForm(raw, "New@Example.com"))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("signup: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.get(t, c, "/settings/profile"); resp.StatusCode != 200 {
		t.Fatalf("new session: %d", resp.StatusCode)
	}
	u, _, err := e.st.UserByEmail(t.Context(), "new@example.com")
	if err != nil || u.VerifiedAt == nil {
		t.Fatalf("user %+v err %v: want verified", u, err)
	}
	if resp, _ := e.get(t, c, "/s/acme-team"); resp.StatusCode != 200 {
		t.Fatalf("derived slug page: %d", resp.StatusCode)
	}
	resp, _ = e.post(t, e.client(t), "/signup", signupForm(raw, "other@example.com"))
	if resp.StatusCode != 404 {
		t.Fatalf("reused invite: %d", resp.StatusCode)
	}
}

func TestInviteSignupFailuresKeepInvite(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	raw := e.invite(t, "")
	c := e.client(t)
	resp, body := e.post(t, c, "/signup", signupForm(raw, opEmail))
	if resp.StatusCode != 422 || !strings.Contains(body, msgEmailTaken) || !strings.Contains(body, raw) {
		t.Fatalf("duplicate email: %d %s", resp.StatusCode, body)
	}
	f := signupForm(raw, "new@example.com")
	f.Set("confirm", "different secret")
	if resp, body := e.post(t, c, "/signup", f); resp.StatusCode != 422 || !strings.Contains(body, msgMismatch) {
		t.Fatalf("mismatch: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.post(t, c, "/signup", signupForm(raw, "new@example.com")); resp.StatusCode != 303 {
		t.Fatalf("corrected: %d %s", resp.StatusCode, body)
	}
}

func TestInviteBoundEmail(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
	raw := e.invite(t, "bound@example.com")
	c := e.client(t)
	resp, body := e.post(t, c, "/signup", signupForm(raw, "other@example.com"))
	if resp.StatusCode != 422 || !strings.Contains(body, msgInviteEmail) {
		t.Fatalf("wrong email: %d %s", resp.StatusCode, body)
	}
	if resp, body := e.post(t, c, "/signup", signupForm(raw, "bound@example.com")); resp.StatusCode != 303 {
		t.Fatalf("right email: %d %s", resp.StatusCode, body)
	}
}

func TestSignupLimits(t *testing.T) {
	t.Run("per address", func(t *testing.T) {
		e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080"}`)
		c := e.client(t)
		for i := 1; i <= 6; i++ {
			f := signupForm(e.invite(t, ""), "x@example.com")
			f.Set("confirm", "mismatch")
			resp, _ := e.post(t, c, "/signup", f)
			want := 422
			if i == 6 {
				want = 429
			}
			if resp.StatusCode != want {
				t.Fatalf("attempt %d: %d, want %d", i, resp.StatusCode, want)
			}
		}
	})
	t.Run("instance wide", func(t *testing.T) {
		e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080","limits":{"signups_per_hour":2}}`)
		for i := 1; i <= 3; i++ {
			c := e.client(t)
			resp, body := e.post(t, c, "/signup", signupForm(e.invite(t, ""), "u"+string(rune('a'+i))+"@example.com"))
			want := 303
			if i == 3 {
				want = 429
			}
			if resp.StatusCode != want {
				t.Fatalf("signup %d: %d %s, want %d", i, resp.StatusCode, body, want)
			}
		}
	})
	t.Run("workspace cap", func(t *testing.T) {
		e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080","limits":{"max_workspaces":1}}`)
		raw := e.invite(t, "")
		resp, body := e.post(t, e.client(t), "/signup", signupForm(raw, "new@example.com"))
		if resp.StatusCode != 403 || !strings.Contains(body, msgNoNewAccounts) {
			t.Fatalf("cap: %d %s", resp.StatusCode, body)
		}
		if _, err := e.st.PeekToken(t.Context(), store.TokenInvite, raw, e.now()); err != nil {
			t.Fatalf("invite consumed by refused sign-up: %v", err)
		}
	})
}

const inviteCfg = `{"signup":"invite","base_url":"http://localhost:8080"}`

// A client with its own remote address, so limits keyed by address differ.
func (e *env) clientFrom(t *testing.T, ip string) *http.Client {
	t.Helper()
	c := e.client(t)
	c.Transport = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
		return d.DialContext(ctx, network, addr)
	}}
	t.Cleanup(c.Transport.(*http.Transport).CloseIdleConnections)
	return c
}

func TestSignupBudgetNeedsValidInvite(t *testing.T) {
	e := newSignupEnv(t, `{"signup":"invite","base_url":"http://localhost:8080","limits":{"signups_per_hour":5}}`)
	a := e.clientFrom(t, "127.0.0.2")
	for i := 0; i < 5; i++ {
		if resp, _ := e.post(t, a, "/signup", signupForm("bogus", "x@example.com")); resp.StatusCode != 404 {
			t.Fatalf("bogus %d: %d", i, resp.StatusCode)
		}
	}
	// Invalid forms behind a valid invite (each from its own address, so only
	// the instance budget could stop them) must not spend the budget of 5.
	inv := e.invite(t, "")
	for i := 0; i < 8; i++ {
		f := signupForm(inv, "x@example.com")
		f.Set("confirm", "nope")
		c := e.clientFrom(t, fmt.Sprintf("127.0.1.%d", i+1))
		if resp, _ := e.post(t, c, "/signup", f); resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("invalid form %d: %d, want 422 (budget spent?)", i, resp.StatusCode)
		}
	}
	b := e.clientFrom(t, "127.0.0.3")
	if resp, body := e.post(t, b, "/signup", signupForm(inv, "new@example.com")); resp.StatusCode != 303 {
		t.Fatalf("valid sign-up after probes: %d %s", resp.StatusCode, body)
	}
}

func TestSignupNoEnumeration(t *testing.T) {
	for _, mode := range []string{"invite", "closed"} {
		e := newSignupEnv(t, `{"signup":"`+mode+`","base_url":"http://localhost:8080"}`)
		c := e.client(t)
		for _, inv := range []string{"", "bogus"} {
			resp, body := e.post(t, c, "/signup", signupForm(inv, opEmail))
			if resp.StatusCode != 404 || strings.Contains(body, msgEmailTaken) {
				t.Fatalf("%s invite %q: %d %s", mode, inv, resp.StatusCode, body)
			}
		}
	}
	e := newSignupEnv(t, inviteCfg)
	c := e.client(t)
	for i := 0; i < 6; i++ {
		if resp, _ := e.post(t, c, "/signup", signupForm("bogus", opEmail)); resp.StatusCode != 404 {
			t.Fatalf("probe %d: %d", i, resp.StatusCode)
		}
	}
	if resp, body := e.post(t, c, "/signup", signupForm(e.invite(t, ""), "new@example.com")); resp.StatusCode != 303 {
		t.Fatalf("per-address slot not refunded: %d %s", resp.StatusCode, body)
	}
}

func TestSignupInvalidFormBogusInviteIs404(t *testing.T) {
	e := newSignupEnv(t, inviteCfg)
	f := signupForm("bogus", "x@example.com")
	f.Set("confirm", "nope")
	if resp, _ := e.post(t, e.client(t), "/signup", f); resp.StatusCode != 404 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}
