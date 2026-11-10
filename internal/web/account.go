package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"uptimemon/internal/password"
)

const (
	msgBadCurrent      = "Current password is incorrect."
	msgPasswordChanged = "Password changed. Other sessions were signed out."
	msgSignedOutAll    = "Signed out everywhere."

	passwordCheckWindow = 15 * time.Minute
	passwordCheckMax    = 5
)

type accountData struct {
	base
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	s.render(w, r, "account", accountData{base: s.base(r, a, "account", "Account")})
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ctx := r.Context()
	d := accountData{base: s.base(r, a, "account", "Account")}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "account", d)
	}
	cur, pw := r.FormValue("current"), r.FormValue("new")
	if err := password.CheckLength(pw); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if pw != r.FormValue("confirm") {
		fail(http.StatusUnprocessableEntity, msgMismatch)
		return
	}
	phc, err := s.sys.PasswordHash(ctx, a.User.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	// The slot is reserved before the hash and refunded unless the guess was
	// wrong, so a user's own correct entries never lock them out.
	key := strconv.FormatInt(a.User.ID, 10)
	if ok, _ := s.passwordCheck.Take(key); !ok {
		s.logger.Warn("password check throttled", "event", "password_check_throttled", "user_id", a.User.ID)
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	ok := false
	// An over-long current password cannot match, so it skips the hash.
	if len(cur) <= 256 {
		ok, err = s.hasher.Verify(ctx, phc, cur)
	}
	if err != nil || ok {
		s.passwordCheck.Undo(key)
	}
	if errors.Is(err, password.ErrBusy) {
		fail(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		fail(http.StatusUnprocessableEntity, msgBadCurrent)
		return
	}
	newPHC, err := s.hasher.Hash(ctx, pw)
	if errors.Is(err, password.ErrBusy) {
		fail(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.sys.SetPassword(ctx, a.User.ID, newPHC, a.sessionHash); err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Info("password changed", "event", "password_changed", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	redirect(w, r, "/account", msgPasswordChanged)
}

func (s *Server) accountSignOutAll(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if err := s.sys.DeleteUserSessions(r.Context(), a.User.ID); err != nil {
		s.fail(w, err)
		return
	}
	s.clearSessionCookie(w, r)
	s.logger.Info("sessions revoked", "event", "sessions_revoked", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	redirect(w, r, "/login", msgSignedOutAll)
}
