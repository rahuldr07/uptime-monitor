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
	if !s.checkCurrent(ctx, w, a, cur, fail) {
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
func (s *Server) checkCurrent(ctx context.Context, w http.ResponseWriter, a *authCtx, cur string, fail func(int, string)) bool {
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
// the rows go in one transaction, and last the workspace is tombstoned so an
// in-flight request cannot restart a runner.
func (s *Server) accountDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ctx := r.Context()
	d := accountData{base: s.base(r, a, "account", "Account"), CanDelete: !a.User.IsOperator}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "account", d)
	}
	if !s.checkCurrent(ctx, w, a, r.FormValue("current"), fail) {
		return
	}
	// Refused before anything is revoked or stopped. DeleteAccount repeats the
	// check in its transaction and stays the authority.
	last, err := s.sys.IsLastOperator(ctx, a.User.ID)
	if err != nil {
		s.fail(w, err)
		return
	}
	if last {
		fail(http.StatusUnprocessableEntity, msgLastOperator)
		return
	}
	if err := s.sys.DeleteUserSessions(ctx, a.User.ID); err != nil {
		s.fail(w, err)
		return
	}
	// Sessions are gone, so the user cannot retry: finish on a context a
	// dropped connection cannot cancel, or the account is left half deleted.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	wsID := a.WS.ID()
	if wsID != store.OperatorWorkspaceID {
		s.mon.RemoveWorkspace(wsID)
	}
	deleted, err := s.sys.DeleteAccount(ctx, a.User.ID)
	if err != nil {
		// Nothing was deleted: bring the runners back so monitoring does not stop silently.
		if wsID != store.OperatorWorkspaceID {
			s.restartRunners(ctx, a.WS)
		}
		if errors.Is(err, store.ErrLastOperator) {
			fail(http.StatusUnprocessableEntity, msgLastOperator)
			return
		}
		s.fail(w, err)
		return
	}
	gone := false
	for _, id := range deleted {
		s.mon.ForgetWorkspace(id)
		s.statusCache.forget(id)
		gone = gone || id == wsID
	}
	if wsID != store.OperatorWorkspaceID && !gone {
		// Stopped above but the workspace survived: bring its runners back.
		s.restartRunners(ctx, a.WS)
	}
	s.clearSessionCookie(w, r)
	s.logger.Info("account deleted", "event", "account_deleted", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	redirect(w, r, "/login", msgAccountDeleted)
}

// restartRunners reloads every target of ws after a failed delete.
func (s *Server) restartRunners(ctx context.Context, ws *store.WS) {
	// The request context may be the reason the delete failed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	ts, err := ws.ListTargets(ctx)
	if err != nil {
		s.logger.Error("restart runners after failed account delete", "workspace", ws.ID(), "err", err)
		return
	}
	for _, t := range ts {
		if err := s.mon.Reload(ctx, t.ID); err != nil {
			s.logger.Error("restart runner after failed account delete", "target", t.ID, "err", err)
		}
	}
}
