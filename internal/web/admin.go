package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"uptimemon/internal/store"
)

const (
	msgUserDisabled    = "User disabled."
	msgUserEnabled     = "User enabled."
	msgUserDeleted     = "User deleted."
	msgNoDisableOp     = "Operators cannot be disabled."
	msgNoDeleteOp      = "Operators cannot be deleted."
	msgNoResetOp       = "Operators cannot be sent a reset link."
	msgNoResetDisabled = "Disabled users cannot be sent a reset link."
	msgDeleteFailed    = "Delete failed; the user is left disabled."
	msgVerifyQueued    = "Verification email queued."
	msgResetQueued     = "Reset email queued."

	adminFieldMax = 200
	adminPageMax  = 1_000_000
)

// adminKeep is the list state a panel form carries in hidden fields and a
// redirect restores. Page is 1-based.
type adminKeep struct {
	Q, Host string
	Page    int
}

func keepFrom(r *http.Request) adminKeep {
	field := func(name string) string {
		v := strings.TrimSpace(r.FormValue(name))
		if len(v) > adminFieldMax {
			v = v[:adminFieldMax]
		}
		return strings.ToValidUTF8(v, "")
	}
	p, _ := strconv.Atoi(r.FormValue("page"))
	return adminKeep{Q: field("q"), Host: field("host"), Page: min(max(p, 1), adminPageMax)}
}

// URL is /admin/users with the kept state; defaults are left out.
func (k adminKeep) URL(page int) string {
	v := url.Values{}
	if k.Q != "" {
		v.Set("q", k.Q)
	}
	if k.Host != "" {
		v.Set("host", k.Host)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if len(v) == 0 {
		return "/admin/users"
	}
	return "/admin/users?" + v.Encode()
}

type adminUsersData struct {
	base
	Rows      []store.UserRow
	Total     int
	Keep      adminKeep
	Prev      string // "" when there is no previous page
	Next      string
	CanMail   bool
	Link      string // one-time reset link; set only on the response that creates it
	LinkEmail string
}

type adminDeleteData struct {
	base
	Target store.User
	Keep   adminKeep
}

// adminDone redirects to the users list the action came from, with a flash.
func adminDone(w http.ResponseWriter, r *http.Request, msg string) {
	redirect(w, r, keepFrom(r).URL(keepFrom(r).Page), msg)
}

// operatorAction logs a successful panel action. target is 0 when none applies.
func (s *Server) operatorAction(r *http.Request, a *authCtx, action string, target int64) {
	args := []any{"event", "operator_action", "user_id", a.User.ID, "action", action}
	if target != 0 {
		args = append(args, "target_user_id", target)
	}
	s.logger.Info("operator action", append(args, "ip", clientIP(r, s.cfg.ProxyHops()))...)
}

// operatorRefused logs a refused panel action and sends the operator back
// with the reason.
func (s *Server) operatorRefused(w http.ResponseWriter, r *http.Request, a *authCtx, action string, target int64, msg string) {
	s.logger.Warn("operator action refused", "event", "operator_action", "user_id", a.User.ID, "action", action,
		"target_user_id", target, "result", "refused", "ip", clientIP(r, s.cfg.ProxyHops()))
	adminDone(w, r, msg)
}

func (s *Server) adminTarget(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	return loadByID(s, w, r, s.sys.UserByID)
}

func (s *Server) adminUsersView(r *http.Request, a *authCtx) (adminUsersData, error) {
	k := keepFrom(r)
	rows, total, err := s.sys.ListUsers(r.Context(), store.UserQuery{Email: k.Q, Host: k.Host, Page: k.Page - 1})
	if err != nil {
		return adminUsersData{}, err
	}
	d := adminUsersData{base: s.base(r, a, "admin", "Users"), Rows: rows, Total: total, Keep: k, CanMail: s.mail != nil}
	if k.Page > 1 {
		d.Prev = k.URL(k.Page - 1)
	}
	if k.Page*store.UsersPerPage < total {
		d.Next = k.URL(k.Page + 1)
	}
	return d, nil
}

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request, a *authCtx) {
	d, err := s.adminUsersView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "admin_users", d)
}

// adminDisable suspends a user's workspaces. ForgetWorkspace waits for
// in-flight alerts (up to about two minutes), so this can be slow; the
// request is detached so a dropped connection cannot stop it half way.
func (s *Server) adminDisable(w http.ResponseWriter, r *http.Request, a *authCtx) {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	id, ok := pathUserID(w, r)
	if !ok {
		return
	}
	ids, err := s.sys.SetUserDisabled(ctx, id, true, s.now())
	switch {
	case isNotFound(err):
		http.NotFound(w, r)
		return
	case isOperatorErr(err):
		s.operatorRefused(w, r, a, "disable", id, msgNoDisableOp)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	for _, wsID := range ids {
		s.mon.ForgetWorkspace(wsID)
	}
	s.operatorAction(r, a, "disable", id)
	adminDone(w, r, msgUserDisabled)
}

// adminEnable lifts a suspension and restarts the user's runners.
func (s *Server) adminEnable(w http.ResponseWriter, r *http.Request, a *authCtx) {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	id, ok := pathUserID(w, r)
	if !ok {
		return
	}
	ids, err := s.sys.SetUserDisabled(ctx, id, false, s.now())
	if isNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, wsID := range ids {
		s.mon.RestoreWorkspace(wsID)
		s.restartRunners(ctx, s.sys.Workspace(wsID))
	}
	s.operatorAction(r, a, "enable", id)
	adminDone(w, r, msgUserEnabled)
}

func (s *Server) adminDeletePage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	u, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if u.IsOperator {
		s.operatorRefused(w, r, a, "delete", u.ID, msgNoDeleteOp)
		return
	}
	s.render(w, r, "admin_delete", adminDeleteData{base: s.base(r, a, "admin", "Delete user"), Target: u, Keep: keepFrom(r)})
}

// adminDelete deletes a user the way the operator panel does: disable first
// (sessions gone, workspaces suspended), stop the runners, then delete the
// rows. A failed delete leaves the user disabled rather than half alive.
func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	u, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if u.IsOperator {
		s.operatorRefused(w, r, a, "delete", u.ID, msgNoDeleteOp)
		return
	}
	d := adminDeleteData{base: s.base(r, a, "admin", "Delete user"), Target: u, Keep: keepFrom(r)}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "admin_delete", d)
	}
	if !s.checkCurrent(r.Context(), w, a, r.FormValue("current"), fail) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	ids, err := s.sys.SetUserDisabled(ctx, u.ID, true, s.now())
	switch {
	case isNotFound(err):
		http.NotFound(w, r)
		return
	case isOperatorErr(err):
		s.operatorRefused(w, r, a, "delete", u.ID, msgNoDeleteOp)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	for _, wsID := range ids {
		s.mon.ForgetWorkspace(wsID)
	}
	deleted, err := s.sys.DeleteAccount(ctx, u.ID)
	if err != nil {
		s.logger.Error("operator delete failed", "err", err, "user_id", a.User.ID, "target_user_id", u.ID)
		adminDone(w, r, msgDeleteFailed)
		return
	}
	for _, wsID := range deleted {
		s.mon.ForgetWorkspace(wsID)
		s.statusCache.forget(wsID)
	}
	s.operatorAction(r, a, "delete", u.ID)
	adminDone(w, r, msgUserDeleted)
}

// adminResend mails a new verification link to an unverified, enabled user.
func (s *Server) adminResend(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if s.mail == nil {
		http.NotFound(w, r)
		return
	}
	u, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if u.VerifiedAt != nil || u.DisabledAt != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.sendVerify(r.Context(), u); err != nil {
		s.fail(w, err)
		return
	}
	s.operatorAction(r, a, "resend_verification", u.ID)
	adminDone(w, r, msgVerifyQueued)
}

// adminResetLink issues a password-reset link for a user: by email when mail
// is configured, otherwise shown once on the response. The link is never put
// in a flash, a URL or a log, and the page is not cached or live-refreshed.
func (s *Server) adminResetLink(w http.ResponseWriter, r *http.Request, a *authCtx) {
	u, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	switch {
	case u.IsOperator:
		s.operatorRefused(w, r, a, "reset_link", u.ID, msgNoResetOp)
		return
	case u.DisabledAt != nil:
		s.operatorRefused(w, r, a, "reset_link", u.ID, msgNoResetDisabled)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if s.mail != nil {
		if err := s.mail.Send(u.Email, func() (string, string, error) {
			raw, err := s.sys.CreateToken(ctx, store.TokenReset, u.ID, resetTTL, s.now())
			if err != nil {
				return "", "", err
			}
			subject, body := resetMail(s.cfg.LinkBase() + "/reset?token=" + raw)
			return subject, body, nil
		}); err != nil {
			s.fail(w, err)
			return
		}
		s.operatorAction(r, a, "reset_link", u.ID)
		adminDone(w, r, msgResetQueued)
		return
	}
	raw, err := s.sys.CreateToken(ctx, store.TokenReset, u.ID, resetTTL, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	d, err := s.adminUsersView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.NoRefresh = true
	d.Link, d.LinkEmail = s.cfg.LinkBase()+"/reset?token="+raw, u.Email
	s.operatorAction(r, a, "reset_link", u.ID)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "admin_users", d)
}

func pathUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return id, true
}

func isNotFound(err error) bool    { return errors.Is(err, store.ErrNotFound) }
func isOperatorErr(err error) bool { return errors.Is(err, store.ErrOperator) }
