package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"uptimemon/internal/password"
	"uptimemon/internal/store"
)

const (
	msgBadLogin = "Email or password is incorrect."
	msgDisabled = "This account is disabled."
	msgBusy     = "The server is busy, please try again."
	msgTooMany  = "Too many attempts. Wait a few minutes and try again."
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

// Sign-in limits. Only failures count, so a user's own successful sign-ins
// never lock them out. The address and pair limits are hard; the per-email
// limit only slows an email down (loginSlowGate), so an attacker spraying one
// account from many addresses cannot lock its owner out.
const (
	loginAddrWindow  = time.Minute
	loginAddrMax     = 5
	loginPairWindow  = 15 * time.Minute
	loginPairMax     = 10
	loginEmailWindow = 15 * time.Minute
	loginEmailMax    = 20
	loginSlowWindow  = 5 * time.Second
	loginSlowMax     = 1

	deviceCookie     = "uptime_device"
	hostDeviceCookie = "__Host-uptime_device"
	deviceMaxAge     = 90 * 24 * time.Hour
)

// deviceMAC is the HMAC proving a browser has signed in as userID before.
func (s *Server) deviceMAC(userID int64) string {
	m := hmac.New(sha256.New, s.deviceKey)
	m.Write([]byte("device|" + strconv.FormatInt(userID, 10)))
	return hex.EncodeToString(m.Sum(nil))
}

// deviceKnown reports whether the request carries a valid device cookie for
// userID. It only ever relaxes the per-email slowdown.
func (s *Server) deviceKnown(r *http.Request, userID int64) bool {
	c, err := r.Cookie(s.deviceCookieName(r))
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(strconv.FormatInt(userID, 10)+"."+s.deviceMAC(userID)))
}

// deviceCookieName follows the session cookie rule: over HTTPS only the
// __Host- name is read or written.
func (s *Server) deviceCookieName(r *http.Request) string {
	if s.cookieName(r) == hostSessionCookie {
		return hostDeviceCookie
	}
	return deviceCookie
}

func (s *Server) setDeviceCookie(w http.ResponseWriter, r *http.Request, userID int64) {
	http.SetCookie(w, &http.Cookie{
		Name: s.deviceCookieName(r), Value: strconv.FormatInt(userID, 10) + "." + s.deviceMAC(userID),
		Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.deviceCookieName(r) == hostDeviceCookie, MaxAge: int(deviceMaxAge.Seconds()),
	})
}

func emailHash(email string) string {
	h := sha256.Sum256([]byte(email))
	return hex.EncodeToString(h[:])[:16]
}

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
	d := loginData{base: s.page(r, "Sign in"), Next: safeNext(r.URL.Query().Get("next"))}
	d.Flash = flashFrom(r)
	s.render(w, r, "login", d)
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	d := loginData{base: s.page(r, "Sign in"), Next: safeNext(r.FormValue("next")), Email: strings.TrimSpace(r.FormValue("email"))}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "login", d)
	}
	addr := clientKey(r, s.cfg.ProxyHops())
	ip := clientIP(r, s.cfg.ProxyHops())
	// An invalid address is keyed by its text, so it is limited like any other
	// and nothing depends on whether the account exists.
	email, emailErr := store.NormalizeEmail(d.Email)
	emailKey := email
	if emailErr != nil {
		emailKey = strings.ToLower(d.Email)
		if len(emailKey) > 254 {
			emailKey = emailKey[:254]
		}
	}
	pairKey := emailKey + "|" + addr
	throttled := func() {
		s.logger.Warn("sign-in throttled", "event", "login_throttled", "ip", ip)
		fail(http.StatusTooManyRequests, msgTooMany)
	}
	// failed keeps the address and pair reservations (they are the hit),
	// records the failure against the email and logs it. userID is 0 when the
	// account is not known.
	refund := true
	failed := func(userID int64) {
		refund = false
		s.loginByEmail.Hit(emailKey)
		if userID != 0 {
			s.logger.Warn("sign-in failed", "event", "login_failed", "ip", ip, "user_id", userID)
		} else {
			s.logger.Warn("sign-in failed", "event", "login_failed", "ip", ip, "email_hash", emailHash(emailKey))
		}
	}
	// Every refusal happens before any hash. The address and pair slots are
	// reserved atomically, so a burst cannot out-run the limit while its
	// hashes are still running. They are refunded unless a guess was made.
	if ok, _ := s.loginByAddr.Take(addr); !ok {
		throttled()
		return
	}
	if ok, _ := s.loginByPair.Take(pairKey); !ok {
		s.loginByAddr.Undo(addr)
		throttled()
		return
	}
	defer func() {
		if refund {
			s.loginByAddr.Undo(addr)
			s.loginByPair.Undo(pairKey)
		}
	}()
	// Over-long passwords fail like any other wrong one, before any lookup,
	// so they cannot tell known addresses from unknown ones.
	if len(r.FormValue("password")) > 256 {
		failed(0)
		fail(http.StatusUnauthorized, msgBadLogin)
		return
	}
	ctx := r.Context()
	// An invalid address follows the same path as an unknown one, so the
	// response and its timing do not reveal which addresses have accounts.
	var u store.User
	var phc string
	err := emailErr
	if err == nil {
		u, phc, err = s.sys.UserByEmail(ctx, email)
	}
	known := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidEmail) {
		s.fail(w, err)
		return
	}
	// A slowed email lets one attempt through per loginSlowWindow, except
	// from a browser that has signed in as that user before. The slowdown
	// itself is keyed by the email text alone.
	if ok, _ := s.loginByEmail.Allow(emailKey); !ok && !(known && s.deviceKnown(r, u.ID)) {
		if ok, _ := s.loginSlow.Take(emailKey); !ok {
			throttled()
			return
		}
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
		if known {
			failed(u.ID)
		} else {
			failed(0)
		}
		fail(http.StatusUnauthorized, msgBadLogin)
		return
	}
	if u.DisabledAt != nil || u.VerifiedAt == nil {
		failed(u.ID)
		if u.DisabledAt != nil {
			fail(http.StatusForbidden, msgDisabled)
		} else {
			fail(http.StatusUnauthorized, msgBadLogin)
		}
		return
	}
	// A new session replaces whichever one the browser presented.
	if old := s.sessionToken(r); old != "" {
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
	s.setDeviceCookie(w, r, u.ID)
	s.logger.Info("sign-in ok", "event", "login_ok", "user_id", u.ID, "ip", ip)
	http.Redirect(w, r, d.Next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if raw := s.sessionToken(r); raw != "" {
		if err := s.sys.DeleteSession(r.Context(), raw); err != nil {
			s.logger.Warn("sign-out: cannot delete session", "err", err)
		}
	}
	s.clearSessionCookie(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
