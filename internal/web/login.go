package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"uptimemon/internal/password"
	"uptimemon/internal/store"
)

const (
	msgBadLogin = "Email or password is incorrect."
	msgDisabled = "This account is disabled."
	msgBusy     = "The server is busy, please try again."
	msgTooMany  = "Too many attempts. Wait a minute and try again."
)

// safeNext only allows plain local paths as post-login redirect targets.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/login") || strings.HasPrefix(next, "/setup") ||
		strings.HasPrefix(next, "/reset") || strings.ContainsAny(next, "\\\r\n\t ") {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	return next
}

// loginMaxFailures is how many failed sign-ins a client may make per minute.
// Successful sign-ins are not counted, so users are never locked out by
// their own use; attackers sharing a user's IP (or proxy, when trust_proxy is
// off) still get throttled.
const loginMaxFailures = 5

// passwordEqual compares in constant time. Hashing first gives both sides
// the same length, since ConstantTimeCompare returns early on a length
// mismatch and would otherwise leak the password's length.
func passwordEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

type loginData struct {
	base
	Next  string
	Email string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "login", loginData{base: s.page(r, "Sign in · Uptime"), Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	d := loginData{base: s.page(r, "Sign in · Uptime"), Next: safeNext(r.FormValue("next")), Email: strings.TrimSpace(r.FormValue("email"))}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "login", d)
	}
	key := clientKey(r, s.cfg.ProxyHops())
	if ok, _ := s.limiter.Allow(key); !ok {
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	ctx := r.Context()
	// An invalid address follows the same path as an unknown one, so the
	// response and its timing do not reveal which addresses have accounts.
	var u store.User
	var phc string
	email, err := store.NormalizeEmail(d.Email)
	if err == nil {
		u, phc, err = s.sys.UserByEmail(ctx, email)
	}
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidEmail) {
		s.fail(w, err)
		return
	}
	var ok bool
	if known {
		ok, err = s.hasher.Verify(ctx, phc, r.FormValue("password"))
	} else {
		err = s.hasher.VerifyDummy(ctx)
	}
	if errors.Is(err, password.ErrBusy) {
		fail(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !known || !ok {
		s.limiter.Hit(key)
		fail(http.StatusUnauthorized, msgBadLogin)
		return
	}
	if u.DisabledAt != nil || u.VerifiedAt == nil {
		s.limiter.Hit(key)
		if u.DisabledAt != nil {
			fail(http.StatusForbidden, msgDisabled)
		} else {
			fail(http.StatusUnauthorized, msgBadLogin)
		}
		return
	}
	// A new session replaces whichever one the browser presented.
	if old := sessionToken(r); old != "" {
		if err := s.sys.DeleteSession(ctx, old); err != nil {
			s.logger.Warn("sign-in: cannot delete previous session", "err", err)
		}
	}
	raw, err := s.sys.CreateSession(ctx, u.ID, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, r, raw)
	http.Redirect(w, r, d.Next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if raw := sessionToken(r); raw != "" {
		if err := s.sys.DeleteSession(r.Context(), raw); err != nil {
			s.logger.Warn("sign-out: cannot delete session", "err", err)
		}
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
