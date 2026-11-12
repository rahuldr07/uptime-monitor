package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/oauth"
	"github.com/rahuldr07/uptime-monitor/internal/oauth/oauthtest"
)

const redirect = "https://status.example.com/auth/x/callback"

// authorizeCode plays the browser: it follows /authorize and returns the code.
func authorizeCode(t *testing.T, p *oauth.Provider, challenge string) string {
	t.Helper()
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noFollow.Get(p.AuthURL("st", challenge, redirect, false))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("state") != "st" || loc.Query().Get("code") == "" {
		t.Fatalf("bad redirect %q", resp.Header.Get("Location"))
	}
	return loc.Query().Get("code")
}

func flow(t *testing.T, f *oauthtest.Fake) (oauth.Identity, error) {
	t.Helper()
	p := f.Provider()
	verifier, challenge := oauth.NewPKCE()
	tok, err := p.Exchange(context.Background(), authorizeCode(t, p, challenge), verifier, redirect)
	if err != nil {
		return oauth.Identity{}, err
	}
	return p.Identity(context.Background(), tok)
}

func TestGoodFlow(t *testing.T) {
	for _, kind := range []string{"google", "github"} {
		f := oauthtest.New(t, kind)
		id, err := flow(t, f)
		if err != nil || id != (oauth.Identity{Subject: "12345", Email: "person@example.com"}) {
			t.Errorf("%s: id = %+v, err = %v", kind, id, err)
		}
	}
}

func TestUnverifiedEmail(t *testing.T) {
	for _, kind := range []string{"google", "github"} {
		f := oauthtest.New(t, kind)
		f.EmailVerified = false
		if _, err := flow(t, f); !errors.Is(err, oauth.ErrNoVerifiedEmail) {
			t.Errorf("%s: err = %v", kind, err)
		}
	}
}

func TestGitHubNeedsPrimaryVerified(t *testing.T) {
	f := oauthtest.New(t, "github")
	f.GitHubEmails = []oauthtest.GitHubEmail{
		{Email: "a@example.com", Primary: true, Verified: false},
		{Email: "b@example.com", Primary: false, Verified: true},
	}
	if _, err := flow(t, f); !errors.Is(err, oauth.ErrNoVerifiedEmail) {
		t.Fatalf("err = %v", err)
	}
	f.GitHubEmails = append(f.GitHubEmails, oauthtest.GitHubEmail{Email: "C@Example.com", Primary: true, Verified: true})
	if id, err := flow(t, f); err != nil || id.Email != "c@example.com" {
		t.Fatalf("id = %+v, err = %v", id, err)
	}
}

func TestBadVerifier(t *testing.T) {
	for _, kind := range []string{"google", "github"} {
		f := oauthtest.New(t, kind)
		p := f.Provider()
		_, challenge := oauth.NewPKCE()
		wrong, _ := oauth.NewPKCE()
		if tok, err := p.Exchange(context.Background(), authorizeCode(t, p, challenge), wrong, redirect); err == nil || tok != "" {
			t.Errorf("%s: tok = %q, err = %v", kind, tok, err)
		}
	}
}

func TestTokenErrors(t *testing.T) {
	f := oauthtest.New(t, "github")
	f.TokenErrorIn200 = true
	_, err := flow(t, f)
	if err == nil || !strings.Contains(err.Error(), "bad_verification_code") || !strings.Contains(err.Error(), "200") {
		t.Errorf("error in 200: %v", err)
	}
	f.TokenErrorIn200, f.FailToken = false, true
	if _, err := flow(t, f); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("500: %v", err)
	}
}

func TestBadSecret(t *testing.T) {
	f := oauthtest.New(t, "google")
	p := oauth.NewForTest("google", "fake-client", "wrong", f.Endpoints(), f.Client())
	verifier, challenge := oauth.NewPKCE()
	if _, err := p.Exchange(context.Background(), authorizeCode(t, p, challenge), verifier, redirect); err == nil {
		t.Fatal("wrong client secret accepted")
	}
}

func TestTimeout(t *testing.T) {
	f := oauthtest.New(t, "google")
	f.Delay = 300 * time.Millisecond
	c := *f.Client()
	c.Timeout = 50 * time.Millisecond
	p := oauth.NewForTest("google", "fake-client", oauthtest.Secret, f.Endpoints(), &c)
	verifier, challenge := oauth.NewPKCE()
	_, err := p.Exchange(context.Background(), authorizeCode(t, p, challenge), verifier, redirect)
	if err == nil || strings.Contains(err.Error(), f.URL) {
		t.Fatalf("err = %v (want a timeout without the URL)", err)
	}
}

func TestNonJSONAndRedirects(t *testing.T) {
	secretBody := "SECRET-BODY-TEXT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/moved" {
			http.Redirect(w, r, "/token", http.StatusFound)
			return
		}
		w.Write([]byte("<html>" + secretBody + "</html>"))
	}))
	defer srv.Close()
	for _, path := range []string{"/token", "/moved"} {
		p := oauth.NewForTest("google", "id", "s", oauth.Endpoints{Token: srv.URL + path, UserInfo: srv.URL + "/info"}, srv.Client())
		_, err := p.Exchange(context.Background(), "c", "v", redirect)
		if err == nil || strings.Contains(err.Error(), secretBody) {
			t.Errorf("%s: token err = %v", path, err)
		}
		if _, err := p.Identity(context.Background(), "tok"); err == nil || strings.Contains(err.Error(), secretBody) {
			t.Errorf("%s: identity err = %v", path, err)
		}
	}
}

func TestGoogleEmailVerifiedForms(t *testing.T) {
	for body, ok := range map[string]bool{
		`{"sub":"1","email":"a@example.com","email_verified":true}`:    true,
		`{"sub":"1","email":"a@example.com","email_verified":"true"}`:  true,
		`{"sub":"1","email":"a@example.com","email_verified":"false"}`: false,
		`{"sub":"1","email":"a@example.com","email_verified":false}`:   false,
		`{"sub":"1","email":"a@example.com"}`:                          false,
		`{"sub":"","email":"a@example.com","email_verified":true}`:     false,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
		p := oauth.NewForTest("google", "id", "s", oauth.Endpoints{UserInfo: srv.URL}, srv.Client())
		id, err := p.Identity(context.Background(), "tok")
		if (err == nil) != ok || (ok && id.Email != "a@example.com") {
			t.Errorf("%s: id = %+v, err = %v", body, id, err)
		}
		srv.Close()
	}
}

func TestAuthURL(t *testing.T) {
	g := oauth.Google("gid", "gs")
	if g.Name() != "google" || g.Label() != "Google" {
		t.Fatalf("%s %s", g.Name(), g.Label())
	}
	for _, reauth := range []bool{false, true} {
		u, err := url.Parse(g.AuthURL("S", "CH", redirect, reauth))
		if err != nil || u.Host != "accounts.google.com" || u.Path != "/o/oauth2/v2/auth" {
			t.Fatalf("url = %v, %v", u, err)
		}
		q := u.Query()
		want := map[string]string{"response_type": "code", "client_id": "gid", "redirect_uri": redirect, "scope": "openid email",
			"state": "S", "code_challenge": "CH", "code_challenge_method": "S256", "prompt": "select_account"}
		for k, v := range want {
			if q.Get(k) != v {
				t.Errorf("google %s = %q, want %q", k, q.Get(k), v)
			}
		}
		if q.Has("max_age") || q.Has("client_secret") {
			t.Errorf("unexpected params: %v", q)
		}
	}
	h := oauth.GitHub("hid", "hs")
	u, _ := url.Parse(h.AuthURL("S", "CH", redirect, false))
	q := u.Query()
	if h.Label() != "GitHub" || u.Host != "github.com" || u.Path != "/login/oauth/authorize" || q.Get("scope") != "user:email" ||
		q.Get("code_challenge_method") != "S256" || q.Get("prompt") != "select_account" || q.Get("allow_signup") != "true" {
		t.Errorf("github url = %v", u)
	}
}

func TestPKCE(t *testing.T) {
	v1, c1 := oauth.NewPKCE()
	v2, _ := oauth.NewPKCE()
	if len(v1) != 43 || len(c1) != 43 || v1 == v2 || v1 == c1 {
		t.Fatalf("v=%q c=%q", v1, c1)
	}
}
