package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"uptimemon/internal/password"
	"uptimemon/internal/store"
)

const (
	msgBadCurrent      = "Current password is incorrect."
	msgPasswordChanged = "Password changed. Other sessions were signed out."
	msgSignedOutAll    = "Signed out everywhere."
	msgLastOperator    = "The last operator can't delete their account."
	msgAccountDeleted  = "Your account was deleted."

	passwordCheckWindow = 15 * time.Minute
	passwordCheckMax    = 5
)

type accountData struct {
	base
	CanDelete bool // operators cannot delete their account here
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	s.render(w, r, "account", accountData{base: s.base(r, a, "account", "Account"), CanDelete: !a.User.IsOperator})
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ctx := r.Context()
	d := accountData{base: s.base(r, a, "account", "Account"), CanDelete: !a.User.IsOperator}
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
	if !s.checkCurrent(w, ctx, a, cur, fail) {
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

// checkCurrent verifies the signed-in user's current password with the
// per-user limiter. On failure it has already answered and reports false.
func (s *Server) checkCurrent(w http.ResponseWriter, ctx context.Context, a *authCtx, cur string, fail func(int, string)) bool {
	phc, err := s.sys.PasswordHash(ctx, a.User.ID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	// The slot is reserved before the hash and refunded unless the guess was
	// wrong, so a user's own correct entries never lock them out.
	key := strconv.FormatInt(a.User.ID, 10)
	if ok, _ := s.passwordCheck.Take(key); !ok {
		s.logger.Warn("password check throttled", "event", "password_check_throttled", "user_id", a.User.ID)
		fail(http.StatusTooManyRequests, msgTooMany)
		return false
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
		return false
	}
	if err != nil {
		s.fail(w, err)
		return false
	}
	if !ok {
		fail(http.StatusUnprocessableEntity, msgBadCurrent)
		return false
	}
	return true
}

// accountDelete deletes the signed-in user, their workspace and all its data.
// Sessions go first so the account cannot be used mid-delete, then the
// workspace's runners stop so nothing writes to rows about to disappear, then
// the rows go in one transaction. An operator's delete is attempted first,
// because it can be refused (last operator) and must then change nothing.
func (s *Server) accountDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ctx := r.Context()
	d := accountData{base: s.base(r, a, "account", "Account"), CanDelete: !a.User.IsOperator}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "account", d)
	}
	if !s.checkCurrent(w, ctx, a, r.FormValue("current"), fail) {
		return
	}
	var deleted []int64
	var err error
	if a.User.IsOperator {
		// Never revoked first: a refused delete must leave the operator signed in.
		if deleted, err = s.sys.DeleteAccount(ctx, a.User.ID); err == nil {
			for _, ws := range deleted {
				s.mon.RemoveWorkspace(ws)
			}
		}
	} else {
		if err = s.sys.DeleteUserSessions(ctx, a.User.ID); err != nil {
			s.fail(w, err)
			return
		}
		if id := a.WS.ID(); id != store.OperatorWorkspaceID {
			s.mon.RemoveWorkspace(id)
		}
		deleted, err = s.sys.DeleteAccount(ctx, a.User.ID)
	}
	if errors.Is(err, store.ErrLastOperator) {
		fail(http.StatusUnprocessableEntity, msgLastOperator)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, id := range deleted {
		s.statusCache.forget(id)
	}
	s.clearSessionCookie(w, r)
	s.logger.Info("account deleted", "event", "account_deleted", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	redirect(w, r, "/login", msgAccountDeleted)
}
