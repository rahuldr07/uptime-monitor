// Package oauth implements the Google and GitHub authorization-code flow with
// PKCE: building the authorization URL, exchanging the code and fetching a
// verified email identity. Nothing here logs or returns response bodies.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/netguard"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// ErrNoVerifiedEmail means the provider account has no verified email address.
var ErrNoVerifiedEmail = errors.New("oauth: no verified email on the provider account")

const maxBody = 1 << 20

// Identity is who the provider says signed in. Email is normalized and verified.
type Identity struct{ Subject, Email string }

// Endpoints are a provider's URLs. Google uses UserInfo; GitHub uses User and Emails.
type Endpoints struct{ Auth, Token, UserInfo, User, Emails string }

// Provider is one social sign-in provider.
type Provider struct {
	name, label, clientID, secret, scopes string
	e                                     Endpoints
	client                                *http.Client
}

var googleEndpoints = Endpoints{
	Auth:     "https://accounts.google.com/o/oauth2/v2/auth",
	Token:    "https://oauth2.googleapis.com/token",
	UserInfo: "https://openidconnect.googleapis.com/v1/userinfo",
}

var githubEndpoints = Endpoints{
	Auth:   "https://github.com/login/oauth/authorize",
	Token:  "https://github.com/login/oauth/access_token",
	User:   "https://api.github.com/user",
	Emails: "https://api.github.com/user/emails?per_page=100",
}

// Google returns the Google provider with its production endpoints.
func Google(clientID, secret string) *Provider {
	return NewForTest("google", clientID, secret, googleEndpoints, netguard.Client(false, 10*time.Second))
}

// GitHub returns the GitHub provider with its production endpoints.
func GitHub(clientID, secret string) *Provider {
	return NewForTest("github", clientID, secret, githubEndpoints, netguard.Client(false, 10*time.Second))
}

// NewForTest builds a provider ("google" or "github") on arbitrary endpoints
// and client. It is exported for tests that point at a fake; production code
// must use Google and GitHub, whose endpoints are fixed and guarded.
func NewForTest(name, clientID, secret string, e Endpoints, c *http.Client) *Provider {
	p := &Provider{name: name, label: name, clientID: clientID, secret: secret, e: e}
	switch name {
	case "google":
		p.label, p.scopes = "Google", "openid email"
	case "github":
		p.label, p.scopes = "GitHub", "user:email"
	}
	// Never follow redirects: a token or bearer call has no business moving.
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	p.client = &cc
	return p
}

// Name is the provider key ("google", "github"); Label is its display name.
func (p *Provider) Name() string  { return p.name }
func (p *Provider) Label() string { return p.label }

// NewPKCE returns a random verifier and its S256 challenge.
func NewPKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL is where to send the browser. Both providers get the account picker,
// since neither documents a way to force a fresh login, so reauth changes nothing.
func (p *Provider) AuthURL(state, codeChallenge, redirectURI string, reauth bool) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {p.scopes},
		"state":                 {state},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	if p.name == "github" {
		q.Set("allow_signup", "true")
	}
	return p.e.Auth + "?" + q.Encode()
}

// Exchange trades an authorization code for an access token.
func (p *Provider) Exchange(ctx context.Context, code, verifier, redirectURI string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {p.clientID},
		"client_secret": {p.secret},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.e.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("oauth: bad token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	status, err := p.do(req, &out)
	if err != nil {
		return "", fmt.Errorf("oauth: token request: %w", err)
	}
	// GitHub reports failures in the body, with an undocumented status.
	if out.Error != "" {
		return "", fmt.Errorf("oauth: token endpoint refused the code (status %d, error %s)", status, kind(out.Error))
	}
	if status != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("oauth: token endpoint gave no token (status %d)", status)
	}
	return out.AccessToken, nil
}

// kind passes through a short snake_case error code and hides anything else.
func kind(s string) string {
	if len(s) > 40 || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz_") != "" {
		return "unrecognized"
	}
	return s
}

// do sends req and decodes at most 1 MiB of JSON into v. It returns the HTTP
// status; the body is never included in an error.
func (p *Provider) do(req *http.Request, v any) (int, error) {
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", "uptime-monitor")
	resp, err := p.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL
		}
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, errors.New("reading response failed")
	}
	if err := json.Unmarshal(body, v); err != nil {
		if resp.StatusCode != http.StatusOK {
			return resp.StatusCode, nil // an error status with a non-JSON body
		}
		return resp.StatusCode, errors.New("response is not valid JSON")
	}
	return resp.StatusCode, nil
}

// get fetches a JSON resource with the access token.
func (p *Provider) get(ctx context.Context, u, token string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return errors.New("oauth: bad request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if p.name == "github" {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	status, err := p.do(req, v)
	if err != nil {
		return fmt.Errorf("oauth: identity request: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("oauth: identity request returned status %d", status)
	}
	return nil
}

// Identity fetches the signed-in account's subject and verified email.
func (p *Provider) Identity(ctx context.Context, accessToken string) (Identity, error) {
	if p.name == "github" {
		return p.githubIdentity(ctx, accessToken)
	}
	var u struct {
		Sub           string          `json:"sub"`
		Email         string          `json:"email"`
		EmailVerified json.RawMessage `json:"email_verified"`
	}
	if err := p.get(ctx, p.e.UserInfo, accessToken, &u); err != nil {
		return Identity{}, err
	}
	// Google documents a bool, but accept the string form too.
	if v := string(u.EmailVerified); v != "true" && v != `"true"` {
		return Identity{}, ErrNoVerifiedEmail
	}
	return finish(u.Sub, u.Email)
}

func (p *Provider) githubIdentity(ctx context.Context, token string) (Identity, error) {
	var u struct {
		ID int64 `json:"id"`
	}
	if err := p.get(ctx, p.e.User, token, &u); err != nil {
		return Identity{}, err
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := p.get(ctx, p.e.Emails, token, &emails); err != nil {
		return Identity{}, err
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			return finish(strconv.FormatInt(u.ID, 10), e.Email)
		}
	}
	return Identity{}, ErrNoVerifiedEmail
}

func finish(sub, email string) (Identity, error) {
	if sub == "" || sub == "0" {
		return Identity{}, errors.New("oauth: provider returned no subject")
	}
	email, err := store.NormalizeEmail(email)
	if err != nil {
		return Identity{}, ErrNoVerifiedEmail
	}
	return Identity{Subject: sub, Email: email}, nil
}
