package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/totp"
)

const (
	msgMFAWrong         = "That code didn't work. Check the time on your phone and try again."
	msgMFAWrongRecovery = "That recovery code didn't work. Each one works once."
	msgSignInToContinue = "Sign in to continue."
	msgMFAExpired       = "That took too long. Sign in again."
	msgMFALimited       = "Too many attempts. Try again later."
	msgMFAError         = "Something went wrong. Try again."

	msgCodeRequired = "Enter a code from your authenticator app."
	msgSetupExpired = "That setup expired. Start again."
	msgMFAAlreadyOn = "Two-factor authentication is already on."
	msgMFANotOn     = "Two-factor authentication is not on."
	msgMFADisabled  = "Two-factor authentication is off. Your other devices were signed out."

	// The one limiter every code check takes a slot from (spec §8.4).
	mfaShortWindow = 15 * time.Minute
	mfaShortMax    = 5
	mfaLongWindow  = 24 * time.Hour
	mfaLongMax     = 20

	// A longer recovery code input is not one, and is not worth hashing.
	maxRecoveryInput = 64

	mfaCookie     = "uptime_mfa"
	hostMFACookie = "__Host-uptime_mfa"
)

// mfaCookieName follows the session cookie rule: over HTTPS only the
// __Host- name is read or written.
func (s *Server) mfaCookieName(r *http.Request) string {
	if s.cookieName(r) == hostSessionCookie {
		return hostMFACookie
	}
	return mfaCookie
}

func (s *Server) setMFACookie(w http.ResponseWriter, r *http.Request, raw string, maxAge int) {
	name := s.mfaCookieName(r)
	http.SetCookie(w, &http.Cookie{Name: name, Value: raw, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: name == hostMFACookie, MaxAge: maxAge})
}

func (s *Server) mfaToken(r *http.Request) string {
	if c, err := r.Cookie(s.mfaCookieName(r)); err == nil {
		return c.Value
	}
	return ""
}

// startSession is the only place a session is created. For an account with
// two-factor on it records a pending sign-in instead and sends the browser to
// /login/2fa; codeOK is true only for the /login/2fa handler after a valid
// code. The browser's previous session is deleted, and the device cookie
// set, only when a session is really created. It writes the response and
// reports whether a session was created.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u store.User, method, next string, codeOK bool) bool {
	ctx := context.WithoutCancel(r.Context())
	next = safeNext(next)
	if u.TOTPEnabledAt != nil && !codeOK {
		// Only the three sign-in methods may wait for a code. Anything else
		// (a link, a sign-up) fails closed: no session, no pending row.
		switch method {
		case "password", "google", "github":
		default:
			s.logger.Error("two-factor account reached startSession by a method that cannot ask for a code", "method", method, "user_id", u.ID)
			redirect(w, r, "/login", msgSignInToContinue)
			return false
		}
		raw, err := s.sys.CreatePendingLogin(ctx, u.ID, method, next, *u.TOTPEnabledAt, s.now())
		if err != nil {
			s.fail(w, err)
			return false
		}
		s.setMFACookie(w, r, raw, int(store.PendingLoginTTL.Seconds()))
		http.Redirect(w, r, "/login/2fa", http.StatusSeeOther)
		return false
	}
	if old := s.sessionToken(r); old != "" {
		if err := s.sys.DeleteSession(ctx, old); err != nil {
			s.logger.Warn("sign-in: cannot delete previous session", "err", err)
		}
	}
	var raw string
	var err error
	if codeOK {
		raw, err = s.sys.CreateSessionMFA(ctx, u.ID, s.now())
	} else {
		raw, err = s.sys.CreateSession(ctx, u.ID, s.now())
	}
	if err != nil {
		s.fail(w, err)
		return false
	}
	s.setSessionCookie(w, r, raw)
	s.setDeviceCookie(w, r, u.ID)
	http.Redirect(w, r, next, http.StatusSeeOther)
	return true
}

// ---- the shared code limiter ----

func userKey(u store.User) string { return strconv.FormatInt(u.ID, 10) }

// takeCode reserves a slot for one code check of u. method is how the person
// got to the code ("password", "google", "github"; "" inside a session). When
// either limit is spent it reports false with the wait; the first time in a
// day it also tells the owner. The device cookie plays no part.
func (s *Server) takeCode(u store.User, method string) (ok bool, wait time.Duration) {
	key := userKey(u)
	ok, wait = s.mfaShort.Take(key)
	if ok {
		if ok, wait = s.mfaLong.Take(key); !ok {
			s.mfaShort.Undo(key)
		}
	}
	if ok {
		return true, 0
	}
	s.warnLimited(u, method)
	return false, wait
}

// warnLimited logs that u's code budget is spent and, the first time in a
// day, tells the owner.
func (s *Server) warnLimited(u store.User, method string) {
	s.logger.Warn("two-factor attempts limited", "event", "mfa_limited", "user_id", u.ID)
	if s.mail == nil {
		return
	}
	if first, _ := s.mfaNotice.Take(userKey(u)); first {
		subject, body := mfaLimitedMail(s.cfg.BaseURL, method, s.now(), u.Location())
		s.mail.Notice(u.Email, subject, body)
	}
}

// codeFailed records a wrong code. If it used the last slot of either window,
// the owner is warned now, not only when a further guess is refused: someone
// who stops at the cap would otherwise never be noticed.
func (s *Server) codeFailed(u store.User, method string, how string) {
	s.logger.Warn("two-factor code wrong", "event", "mfa_failed", "user_id", u.ID, "via", how)
	key := userKey(u)
	okShort, _ := s.mfaShort.Allow(key)
	okLong, _ := s.mfaLong.Allow(key)
	if !okShort || !okLong {
		s.warnLimited(u, method)
	}
}

// refundCode gives the slot back after a good code or a server error.
func (s *Server) refundCode(u store.User) {
	s.mfaShort.Undo(userKey(u))
	s.mfaLong.Undo(userKey(u))
}

// limitedPrefix starts every limiter message built by limitedMessage.
const limitedPrefix = "Too many attempts. Try again in about"

func limitedMessage(wait time.Duration) string {
	m := int((wait + time.Minute - 1) / time.Minute)
	if m < 1 {
		m = 1
	}
	unit := "minutes"
	if m == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("%s %d %s.", limitedPrefix, m, unit)
}

// codeStatus is the HTTP status for a checkCode message.
func codeStatus(msg string) int {
	switch {
	case strings.HasPrefix(msg, limitedPrefix):
		return http.StatusTooManyRequests
	case msg == msgMFAError:
		return http.StatusInternalServerError
	}
	return http.StatusUnauthorized
}

// sixDigits strips white space and reports whether what is left is exactly
// six ASCII digits.
func sixDigits(in string) (string, bool) {
	if len(in) > 64 {
		return "", false
	}
	out := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, in)
	if len(out) != 6 {
		return "", false
	}
	for i := 0; i < len(out); i++ {
		if out[i] < '0' || out[i] > '9' {
			return "", false
		}
	}
	return out, true
}

// verifyCode checks one code or recovery code of u. Every success consumes
// the code atomically (a TOTP step or a recovery code row).
func (s *Server) verifyCode(ctx context.Context, u store.User, code string, recovery bool) (bool, error) {
	if recovery {
		if len(code) > maxRecoveryInput || len(totp.NormalizeRecovery(code)) != 16 {
			return false, nil
		}
		return s.sys.UseRecoveryCode(ctx, u.ID, totp.HashRecovery(code), s.now())
	}
	digits, ok := sixDigits(code)
	if !ok {
		return false, nil
	}
	secret, enabled, err := s.sys.TOTPSecret(ctx, u.ID)
	if err != nil || !enabled {
		return false, err
	}
	step, ok := totp.Match(secret, digits, s.now())
	if !ok {
		return false, nil
	}
	return s.sys.AcceptTOTPStep(ctx, u.ID, secret, step)
}

// checkWith is the one place a code check is gated: it takes a slot from the
// per-user limiter first, runs verify, refunds the slot on success (or a
// server error) and warns the owner when a window fills. checkCode and the
// setup confirmation both go through it.
func (s *Server) checkWith(u store.User, method string, recovery bool, verify func() (bool, error)) (ok bool, msg string) {
	if allowed, wait := s.takeCode(u, method); !allowed {
		return false, limitedMessage(wait)
	}
	good, err := verify()
	if err != nil {
		s.refundCode(u)
		s.logger.Error("two-factor check failed", "err", err, "user_id", u.ID)
		return false, msgMFAError
	}
	if !good {
		how := "code"
		if recovery {
			how = "recovery"
		}
		s.codeFailed(u, method, how)
		if recovery {
			return false, msgMFAWrongRecovery
		}
		return false, msgMFAWrong
	}
	s.refundCode(u)
	return true, ""
}

// checkCode checks a code or recovery code of u against the stored secret or
// recovery codes. On a recovery code it tells the owner how many are left.
// method as for takeCode.
func (s *Server) checkCode(ctx context.Context, u store.User, code string, recovery bool, method string) (ok bool, msg string) {
	ok, msg = s.checkWith(u, method, recovery, func() (bool, error) { return s.verifyCode(ctx, u, code, recovery) })
	if ok && recovery {
		s.recoveryUsed(ctx, u, method != "")
	}
	return ok, msg
}

func (s *Server) recoveryUsed(ctx context.Context, u store.User, signIn bool) {
	left, err := s.sys.RecoveryCodesLeft(ctx, u.ID)
	if err != nil {
		s.logger.Error("two-factor: cannot count recovery codes", "err", err, "user_id", u.ID)
	}
	s.logger.Warn("recovery code used", "event", "mfa_recovery_used", "user_id", u.ID, "left", left)
	if s.mail != nil {
		subject, body := recoveryUsedMail(s.cfg.BaseURL, left, signIn, s.now(), u.Location())
		s.mail.Notice(u.Email, subject, body)
	}
}

// ---- /login/2fa ----

type login2FAData struct {
	base
	Recovery bool
}

func (s *Server) login2FAView(r *http.Request) login2FAData {
	return login2FAData{base: s.page(r, "Two-factor authentication"), Recovery: r.URL.Query().Get("recovery") == "1" || r.FormValue("recovery") == "1"}
}

// backToSignIn drops the pending cookie and shows the sign-in page with msg
// as a persistent error (a flash would fade before it is read).
func (s *Server) backToSignIn(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.setMFACookie(w, r, "", -1)
	d := loginData{base: s.page(r, "Sign in"), SignupMode: s.cfg.Signup, Mail: s.mail != nil, Providers: s.oauthLinks("", "")}
	d.Error = msg
	s.renderStatus(w, r, code, "login", d)
}

func (s *Server) login2FAPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tok := s.mfaToken(r)
	if tok == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if _, err := s.sys.PendingLogin(r.Context(), tok, s.now()); errors.Is(err, store.ErrNotFound) {
		s.backToSignIn(w, r, http.StatusUnauthorized, msgMFAExpired)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "login2fa", s.login2FAView(r))
}

func (s *Server) login2FAPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	tok := s.mfaToken(r)
	if tok == "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	d := s.login2FAView(r)
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "login2fa", d)
	}
	// Each try also counts against the address, like a password guess.
	addr, ip := clientKey(r, s.cfg.ProxyHops()), clientIP(r, s.cfg.ProxyHops())
	if ok, _ := s.loginByAddr.Take(addr); !ok {
		s.logger.Warn("sign-in throttled", "event", "login_throttled", "ip", ip)
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	refund := true
	defer func() {
		if refund {
			s.loginByAddr.Undo(addr)
		}
	}()
	p, err := s.sys.TryPendingLogin(ctx, tok, s.now())
	if errors.Is(err, store.ErrNotFound) {
		// A row that is alive but out of tries is a different story from one
		// that is gone.
		if _, perr := s.sys.PendingLogin(ctx, tok, s.now()); perr == nil {
			refund = false
			s.backToSignIn(w, r, http.StatusTooManyRequests, msgMFALimited)
			return
		}
		s.backToSignIn(w, r, http.StatusUnauthorized, msgMFAExpired)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.sys.UserByID(ctx, p.UserID)
	if errors.Is(err, store.ErrNotFound) {
		s.backToSignIn(w, r, http.StatusUnauthorized, msgMFAExpired)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// Reset, turned off or turned on again, or the account disabled, while
	// the person sat here: never sign in without the code they were promised.
	if u.TOTPEnabledAt == nil || !u.TOTPEnabledAt.Equal(p.TOTPEnabledAt) || u.DisabledAt != nil || u.VerifiedAt == nil {
		_ = s.sys.DeletePendingLogin(ctx, tok)
		s.backToSignIn(w, r, http.StatusUnauthorized, msgMFAExpired)
		return
	}
	recovery := r.FormValue("recovery") == "1"
	good, msg := s.checkCode(ctx, u, r.FormValue("code"), recovery, p.Method)
	if !good {
		refund = false
		fail(codeStatus(msg), msg)
		return
	}
	if err := s.sys.FinishPendingLogin(ctx, tok, s.now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.backToSignIn(w, r, http.StatusUnauthorized, msgMFAExpired)
			return
		}
		s.fail(w, err)
		return
	}
	s.setMFACookie(w, r, "", -1)
	if s.startSession(w, r, u, p.Method, p.Next, true) {
		if p.Method == "password" {
			s.logger.Info("sign-in ok", "event", "login_ok", "user_id", u.ID, "ip", ip, "mfa", true)
		} else {
			s.logger.Info("oauth sign-in ok", "event", "oauth_login_ok", "provider", p.Method, "user_id", u.ID, "ip", ip, "mfa", true)
		}
	}
}

// login2FACancel ends a pending sign-in.
func (s *Server) login2FACancel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if tok := s.mfaToken(r); tok != "" {
		if err := s.sys.DeletePendingLogin(r.Context(), tok); err != nil {
			s.logger.Warn("two-factor: cannot delete pending sign-in", "err", err)
		}
	}
	s.setMFACookie(w, r, "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
