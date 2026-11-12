package web

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/oauth"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	oauthCookie     = "uptime_oauth"
	hostOAuthCookie = "__Host-uptime_oauth"
	oauthTTL        = 10 * time.Minute
	oauthPerMinute  = 20
	oauthWorkspace  = "My workspace"

	msgOAuthFailed     = "Sign-in failed. Try again."
	msgOAuthInviteOnly = "Sign-up on this instance is by invitation."
	msgOAuthExists     = "An account with this email already exists. Sign in with your password (or use Forgot password if you don't have one), then connect %s from your Account page."
	msgOAuthNoEmail    = "Your %s account has no verified email address."
	msgOAuthTaken      = "This %s account is already connected to a different account."
	msgOAuthHave       = "You already have a %s account connected. Disconnect it first."
	msgOAuthReauth     = "Sign in with the %s account connected to this account."
)

// oauthTimeout bounds the code exchange and identity fetch together.
var oauthTimeout = 10 * time.Second

// oauthState is the signed cookie that carries a flow from start to callback.
type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Provider string `json:"p"`
	Intent   string `json:"i"`
	Next     string `json:"n,omitempty"`
	Invite   string `json:"t,omitempty"`
	UserID   int64  `json:"u,omitempty"`
	Issued   int64  `json:"a"` // unix seconds
}

func (s *Server) oauthMAC(b []byte) []byte {
	m := hmac.New(sha256.New, s.oauthKey)
	m.Write(b)
	return m.Sum(nil)
}

func (s *Server) sealOAuth(st oauthState) string {
	b, _ := json.Marshal(st)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(s.oauthMAC(b))
}

func (s *Server) openOAuth(raw string) (oauthState, bool) {
	var st oauthState
	p, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return st, false
	}
	b, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, s.oauthMAC(b)) || json.Unmarshal(b, &st) != nil {
		return st, false
	}
	age := s.now().Sub(time.Unix(st.Issued, 0))
	if age > oauthTTL || age < -time.Minute {
		return st, false
	}
	return st, true
}

// oauthCookieName follows the session cookie rule.
func (s *Server) oauthCookieName(r *http.Request) string {
	if s.cookieName(r) == hostSessionCookie {
		return hostOAuthCookie
	}
	return oauthCookie
}

func (s *Server) setOAuthCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	name := s.oauthCookieName(r)
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: name == hostOAuthCookie, MaxAge: maxAge})
}

func (s *Server) oauthRedirectURI(p *oauth.Provider) string {
	return strings.TrimRight(s.cfg.LinkBase(), "/") + "/auth/" + p.Name() + "/callback"
}

// oauthLink is a sign-in button on the login or sign-up page.
type oauthLink struct{ Label, Href string }

// oauthLinks lists a start link per enabled provider. next and invite are
// carried when set.
func (s *Server) oauthLinks(next, invite string) []oauthLink {
	var out []oauthLink
	for _, p := range s.oauthList {
		q := url.Values{"intent": {"login"}}
		if next != "" && next != "/" {
			q.Set("next", next)
		}
		if invite != "" {
			q.Set("invite", invite)
		}
		out = append(out, oauthLink{p.Label(), "/auth/" + p.Name() + "/start?" + q.Encode()})
	}
	return out
}

// oauthStart sends the browser to the provider. It is a plain link, since the
// CSP only allows same-origin form posts.
func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := s.oauth[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	intent := q.Get("intent")
	if intent == "" {
		intent = "login"
	}
	var uid int64
	switch intent {
	case "login":
	case "link", "reauth":
		a, ok := s.currentAuth(r)
		if !ok {
			http.NotFound(w, r)
			return
		}
		uid = a.User.ID
	default:
		http.NotFound(w, r)
		return
	}
	invite := q.Get("invite")
	if len(invite) > 128 {
		http.NotFound(w, r)
		return
	}
	next := ""
	if n := q.Get("next"); n != "" && len(n) <= 512 {
		next = safeNext(n)
	}
	if ok, _ := s.oauthByAddr.Take(clientKey(r, s.cfg.ProxyHops())); !ok {
		s.oauthRateLimited(w, r)
		return
	}
	state := randomToken()
	verifier, challenge := oauth.NewPKCE()
	s.setOAuthCookie(w, r, s.sealOAuth(oauthState{State: state, Verifier: verifier, Provider: p.Name(), Intent: intent,
		Next: next, Invite: invite, UserID: uid, Issued: s.now().Unix()}), int(oauthTTL.Seconds()))
	http.Redirect(w, r, p.AuthURL(state, challenge, s.oauthRedirectURI(p), intent == "reauth"), http.StatusSeeOther)
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) oauthRateLimited(w http.ResponseWriter, r *http.Request) {
	s.logger.Warn("oauth throttled", "event", "oauth_failed", "reason", "rate_limit", "ip", clientIP(r, s.cfg.ProxyHops()))
	d := loginData{base: s.page(r, "Sign in"), SignupMode: s.cfg.Signup, Mail: s.mail != nil, Providers: s.oauthLinks("", "")}
	d.Error = msgTooMany
	s.renderStatus(w, r, http.StatusTooManyRequests, "login", d)
}

// oauthReq is one callback in flight.
type oauthReq struct {
	s  *Server
	w  http.ResponseWriter
	r  *http.Request
	p  *oauth.Provider
	st oauthState
	id oauth.Identity
	a  *authCtx // the current session, if any
	ip string
}

// fail logs the reason and shows msg. Link and reauth flows of a signed-in
// user stay on the account page.
func (q *oauthReq) fail(code int, reason, msg string) {
	q.s.logger.Warn("oauth failed", "event", "oauth_failed", "provider", q.p.Name(), "reason", reason, "ip", q.ip)
	if q.a != nil && (q.st.Intent == "link" || q.st.Intent == "reauth") {
		d := accountData{base: q.s.base(q.r, q.a, "account", "Account"), CanDelete: !q.a.User.IsOperator}
		d.Error = msg
		q.s.renderStatus(q.w, q.r, code, "account", d)
		return
	}
	d := loginData{base: q.s.page(q.r, "Sign in"), SignupMode: q.s.cfg.Signup, Mail: q.s.mail != nil, Providers: q.s.oauthLinks("", "")}
	d.Error = msg
	q.s.renderStatus(q.w, q.r, code, "login", d)
}

func (q *oauthReq) serverErr(err error) { q.s.fail(q.w, err) }

func (s *Server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	p := s.oauth[r.PathValue("provider")]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	if ok, _ := s.oauthByAddr.Take(clientKey(r, s.cfg.ProxyHops())); !ok {
		s.oauthRateLimited(w, r)
		return
	}
	q := &oauthReq{s: s, w: w, r: r, p: p, ip: clientIP(r, s.cfg.ProxyHops())}
	// The cookie is used once: it is deleted whatever happens next.
	var st oauthState
	valid := false
	if c, err := r.Cookie(s.oauthCookieName(r)); err == nil {
		st, valid = s.openOAuth(c.Value)
	}
	s.setOAuthCookie(w, r, "", -1)
	if !valid {
		q.fail(http.StatusBadRequest, "cookie", msgOAuthFailed)
		return
	}
	q.st = st
	if a, ok := s.currentAuth(r); ok {
		q.a = a
	}
	query := r.URL.Query()
	switch {
	case st.Provider != p.Name():
		q.fail(http.StatusBadRequest, "provider", msgOAuthFailed)
		return
	case subtle.ConstantTimeCompare([]byte(st.State), []byte(query.Get("state"))) != 1 || st.State == "":
		q.fail(http.StatusBadRequest, "state", msgOAuthFailed)
		return
	case query.Get("error") != "":
		q.fail(http.StatusBadRequest, "provider_error", msgOAuthFailed)
		return
	case query.Get("code") == "":
		q.fail(http.StatusBadRequest, "no_code", msgOAuthFailed)
		return
	}
	if st.Intent == "link" || st.Intent == "reauth" {
		if q.a == nil || q.a.User.ID != st.UserID {
			q.fail(http.StatusBadRequest, "session", msgOAuthFailed)
			return
		}
	} else if st.Intent != "login" {
		q.fail(http.StatusBadRequest, "intent", msgOAuthFailed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), oauthTimeout)
	tok, err := p.Exchange(ctx, query.Get("code"), st.Verifier, s.oauthRedirectURI(p))
	if err != nil {
		cancel()
		s.logger.Warn("oauth exchange", "provider", p.Name(), "err", err)
		q.fail(http.StatusBadRequest, "exchange", msgOAuthFailed)
		return
	}
	id, err := p.Identity(ctx, tok)
	cancel()
	if errors.Is(err, oauth.ErrNoVerifiedEmail) {
		q.fail(http.StatusBadRequest, "no_verified_email", fmt.Sprintf(msgOAuthNoEmail, p.Label()))
		return
	}
	if err != nil {
		s.logger.Warn("oauth identity", "provider", p.Name(), "err", err)
		q.fail(http.StatusBadRequest, "identity", msgOAuthFailed)
		return
	}
	q.id = id

	ctx = r.Context()
	owner, err := s.sys.IdentityUser(ctx, p.Name(), id.Subject)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		q.serverErr(err)
		return
	}
	linked := err == nil
	switch st.Intent {
	case "login":
		if linked {
			q.signIn(owner, "oauth_login_ok")
		} else {
			q.unlinkedLogin()
		}
	case "link":
		q.link(owner, linked)
	case "reauth":
		q.reauth(owner, linked)
	}
}

// signIn replaces the browser's session with one for u.
func (q *oauthReq) signIn(u store.User, event string) {
	s, ctx := q.s, q.r.Context()
	if u.DisabledAt != nil {
		q.fail(http.StatusForbidden, "disabled", msgDisabled)
		return
	}
	ctx = context.WithoutCancel(ctx)
	if err := s.sys.TouchIdentity(ctx, q.p.Name(), q.id.Subject, s.now()); err != nil {
		s.logger.Warn("oauth: cannot update last use", "err", err)
	}
	if old := s.sessionToken(q.r); old != "" {
		if err := s.sys.DeleteSession(ctx, old); err != nil {
			s.logger.Warn("oauth: cannot delete previous session", "err", err)
		}
	}
	raw, err := s.sys.CreateSession(ctx, u.ID, s.now())
	if err != nil {
		q.serverErr(err)
		return
	}
	s.setSessionCookie(q.w, q.r, raw)
	s.setDeviceCookie(q.w, q.r, u.ID)
	if event != "" {
		s.logger.Info("oauth sign-in ok", "event", event, "provider", q.p.Name(), "user_id", u.ID, "ip", q.ip)
	}
	next := "/"
	if q.st.Next != "" {
		next = safeNext(q.st.Next)
	}
	http.Redirect(q.w, q.r, next, http.StatusSeeOther)
}

func (q *oauthReq) unlinkedLogin() {
	s, ctx := q.s, q.r.Context()
	email := q.id.Email
	if _, _, err := s.sys.UserByEmail(ctx, email); err == nil {
		s.logger.Warn("oauth sign-in not linked", "event", "oauth_login_unlinked", "provider", q.p.Name(), "ip", q.ip, "email_hash", emailHash(email))
		q.fail(http.StatusConflict, "unlinked", fmt.Sprintf(msgOAuthExists, q.p.Label()))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		q.serverErr(err)
		return
	}
	q.signUp()
}

func (q *oauthReq) signUp() {
	s, ctx := q.s, q.r.Context()
	invite := q.st.Invite
	if invite == "" {
		if s.cfg.Signup != "open" {
			q.fail(http.StatusForbidden, "closed", msgOAuthInviteOnly)
			return
		}
	} else {
		tok, err := s.sys.PeekToken(ctx, store.TokenInvite, invite, s.now())
		if errors.Is(err, store.ErrNotFound) {
			q.fail(http.StatusForbidden, "bad_invite", msgBadInvite)
			return
		}
		if err != nil {
			q.serverErr(err)
			return
		}
		if tok.Email != "" && tok.Email != q.id.Email {
			q.fail(http.StatusForbidden, "invite_email", msgInviteEmail)
			return
		}
	}
	if ok, _ := s.signupByAddr.Take(clientKey(q.r, s.cfg.ProxyHops())); !ok {
		q.fail(http.StatusTooManyRequests, "signup_limit", msgTooMany)
		return
	}
	if ok, _ := s.signupGlobal.Take("all"); !ok {
		q.fail(http.StatusTooManyRequests, "signup_limit", msgTooMany)
		return
	}
	ctx = context.WithoutCancel(ctx)
	u, _, err := s.sys.CreateAccount(ctx, store.AccountInput{Email: q.id.Email, WorkspaceName: oauthWorkspace,
		Verified: true, InviteToken: invite, MaxWorkspaces: *s.cfg.Limits.MaxWorkspaces, Now: s.now(),
		Identity: &store.NewIdentity{Provider: q.p.Name(), Subject: q.id.Subject, Email: q.id.Email}})
	switch {
	case errors.Is(err, store.ErrWorkspaceCap):
		q.fail(http.StatusForbidden, "workspace_cap", msgNoNewAccounts)
		return
	case errors.Is(err, store.ErrEmailTaken):
		q.fail(http.StatusConflict, "unlinked", fmt.Sprintf(msgOAuthExists, q.p.Label()))
		return
	case errors.Is(err, store.ErrInviteEmail):
		q.fail(http.StatusForbidden, "invite_email", msgInviteEmail)
		return
	case errors.Is(err, store.ErrNotFound):
		q.fail(http.StatusForbidden, "bad_invite", msgBadInvite)
		return
	case errors.Is(err, store.ErrIdentityTaken):
		q.fail(http.StatusBadRequest, "identity_taken", msgOAuthFailed)
		return
	case err != nil:
		q.serverErr(err)
		return
	}
	s.logger.Info("oauth sign-up", "event", "oauth_signup", "provider", q.p.Name(), "user_id", u.ID, "ip", q.ip)
	q.signIn(u, "")
}

func (q *oauthReq) link(owner store.User, linked bool) {
	s := q.s
	switch {
	case linked && owner.ID == q.a.User.ID:
		redirect(q.w, q.r, "/account", q.p.Label()+" is already connected.")
		return
	case linked:
		q.fail(http.StatusConflict, "identity_taken", fmt.Sprintf(msgOAuthTaken, q.p.Label()))
		return
	}
	err := s.sys.LinkIdentity(context.WithoutCancel(q.r.Context()), q.a.User.ID, q.p.Name(), q.id.Subject, q.id.Email, s.now())
	if errors.Is(err, store.ErrIdentityTaken) {
		q.fail(http.StatusConflict, "identity_taken", fmt.Sprintf(msgOAuthHave, q.p.Label()))
		return
	}
	if err != nil {
		q.serverErr(err)
		return
	}
	s.logger.Info("oauth linked", "event", "oauth_link", "provider", q.p.Name(), "user_id", q.a.User.ID, "ip", q.ip)
	redirect(q.w, q.r, "/account", q.p.Label()+" connected.")
}

func (q *oauthReq) reauth(owner store.User, linked bool) {
	s := q.s
	if !linked || owner.ID != q.a.User.ID {
		q.fail(http.StatusBadRequest, "reauth_mismatch", fmt.Sprintf(msgOAuthReauth, q.p.Label()))
		return
	}
	if err := s.sys.MarkReauth(context.WithoutCancel(q.r.Context()), q.a.sessionHash, s.now()); err != nil {
		q.serverErr(err)
		return
	}
	s.logger.Info("oauth reauth", "event", "oauth_reauth", "provider", q.p.Name(), "user_id", q.a.User.ID, "ip", q.ip)
	next := "/account"
	if q.st.Next != "" {
		next = safeNext(q.st.Next)
	}
	http.Redirect(q.w, q.r, next, http.StatusSeeOther)
}
