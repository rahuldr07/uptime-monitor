// Package oauthtest is an httptest stand-in for Google and GitHub sign-in.
package oauthtest

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/oauth"
)

// Secret is the client secret the fake's token endpoint expects.
const Secret = "fake-secret"

// GitHubEmail is one entry of the GitHub emails endpoint.
type GitHubEmail struct {
	Email             string `json:"email"`
	Primary, Verified bool
}

// Fake is a running fake provider. Set the exported fields before the flow starts.
type Fake struct {
	URL  string
	Kind string // "google" or "github"

	Subject       string
	Email         string
	EmailVerified bool
	GitHubEmails  []GitHubEmail // when nil, one primary entry built from Email and EmailVerified

	FailToken       bool          // token endpoint answers 500
	TokenErrorIn200 bool          // token endpoint answers 200 with an error body
	Delay           time.Duration // added to every token response

	srv *httptest.Server
	mu  sync.Mutex
	// code -> PKCE challenge, and the access tokens issued
	codes  map[string]string
	tokens map[string]bool
	n      int
}

// New starts a fake of the given kind and stops it when the test ends.
func New(t *testing.T, kind string) *Fake {
	f := &Fake{Kind: kind, Subject: "12345", Email: "Person@Example.com", EmailVerified: true,
		codes: map[string]string{}, tokens: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/authorize", f.authorize)
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/userinfo", f.userinfo)
	mux.HandleFunc("/user", f.user)
	mux.HandleFunc("/user/emails", f.emails)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	return f
}

// Endpoints points a provider at the fake.
func (f *Fake) Endpoints() oauth.Endpoints {
	return oauth.Endpoints{Auth: f.URL + "/authorize", Token: f.URL + "/token",
		UserInfo: f.URL + "/userinfo", User: f.URL + "/user", Emails: f.URL + "/user/emails?per_page=100"}
}

// Client is a plain client for the fake (not netguarded; test use only).
func (f *Fake) Client() *http.Client { return f.srv.Client() }

// Provider is a provider of the fake's kind wired to it.
func (f *Fake) Provider() *oauth.Provider {
	return oauth.NewForTest(f.Kind, "fake-client", Secret, f.Endpoints(), f.Client())
}

// authorize records the PKCE challenge and bounces back with a one-use code.
func (f *Fake) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.n++
	code := "code" + strconv.Itoa(f.n)
	f.codes[code] = q.Get("code_challenge")
	f.mu.Unlock()
	back, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := back.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	back.RawQuery = v.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (f *Fake) token(w http.ResponseWriter, r *http.Request) {
	time.Sleep(f.Delay)
	w.Header().Set("Content-Type", "application/json")
	if f.FailToken {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"server_error"}`))
		return
	}
	if f.TokenErrorIn200 {
		w.Write([]byte(`{"error":"bad_verification_code","error_description":"nope"}`))
		return
	}
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	challenge, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code")) // one use
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	okPKCE := subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
	okSecret := subtle.ConstantTimeCompare([]byte(r.PostForm.Get("client_secret")), []byte(Secret)) == 1
	if !ok || !okPKCE || !okSecret || r.PostForm.Get("grant_type") != "authorization_code" {
		if f.Kind == "github" { // GitHub answers 200 with an error body
			w.Write([]byte(`{"error":"bad_verification_code"}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	tok := "tok-" + r.PostForm.Get("code")
	f.mu.Lock()
	f.tokens[tok] = true
	f.mu.Unlock()
	json.NewEncoder(w).Encode(map[string]string{"access_token": tok, "token_type": "bearer"})
}

// authed checks the bearer token (and, for GitHub, its required headers).
func (f *Fake) authed(w http.ResponseWriter, r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	ok := f.tokens[tok]
	f.mu.Unlock()
	if ok && f.Kind == "github" && (r.Header.Get("User-Agent") == "" ||
		r.Header.Get("Accept") != "application/vnd.github+json" || r.Header.Get("X-GitHub-Api-Version") == "") {
		ok = false
	}
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
	return ok
}

func (f *Fake) userinfo(w http.ResponseWriter, r *http.Request) {
	if f.Kind != "google" || !f.authed(w, r) {
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"sub": f.Subject, "email": f.Email, "email_verified": f.EmailVerified})
}

func (f *Fake) user(w http.ResponseWriter, r *http.Request) {
	if f.Kind != "github" || !f.authed(w, r) {
		return
	}
	id, _ := strconv.ParseInt(f.Subject, 10, 64)
	json.NewEncoder(w).Encode(map[string]any{"id": id, "login": "someone"})
}

func (f *Fake) emails(w http.ResponseWriter, r *http.Request) {
	if f.Kind != "github" || !f.authed(w, r) {
		return
	}
	list := f.GitHubEmails
	if list == nil {
		list = []GitHubEmail{{Email: f.Email, Primary: true, Verified: f.EmailVerified}}
	}
	json.NewEncoder(w).Encode(list)
}
