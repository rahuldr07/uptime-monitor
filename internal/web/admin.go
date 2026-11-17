package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
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
	msgVerifyQueued    = "Verification email queued (subject to mail limits)."
	msgResetQueued     = "Reset email queued (subject to mail limits)."
	msgInviteRevoked   = "Invite revoked."
	msgInviteNotFound  = "Invite not found."

	inviteTTL = 7 * 24 * time.Hour

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
	Badges    map[int64][]string // connected provider labels per user ID
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
	Target      store.User
	Keep        adminKeep
	HasPassword bool        // the operator confirms with their password
	NeedReauth  bool        // a social-only operator without a recent sign-in
	Reauth      []oauthLink // sign-in-again links for that case
}

// adminDeleteView builds the delete page's view model.
func (s *Server) adminDeleteView(r *http.Request, a *authCtx, u store.User) (adminDeleteData, error) {
	ids, err := s.myIdentities(r.Context(), a.User.ID)
	if err != nil {
		return adminDeleteData{}, err
	}
	d := adminDeleteData{base: s.base(r, a, "admin", "Delete user"), Target: u, Keep: keepFrom(r), HasPassword: a.HasPassword}
	d.NeedReauth, d.Reauth = s.reauthState(a, ids, d.Keep.DeleteURL(u.ID))
	return d, nil
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
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	provs, err := s.sys.IdentityProviders(r.Context(), ids) // one query for the page
	if err != nil {
		return adminUsersData{}, err
	}
	d.Badges = map[int64][]string{}
	for id, ps := range provs {
		for _, p := range ps {
			d.Badges[id] = append(d.Badges[id], providerLabel(p))
		}
	}
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
	extendDeadline(w)
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	id, ok := s.pathUserID(w, r)
	if !ok {
		return
	}
	ids, err := s.sys.SetUserDisabled(ctx, id, true, s.now())
	switch {
	case isNotFound(err):
		s.notFound(w, r)
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
	extendDeadline(w)
	s.adminMu.Lock()
	defer s.adminMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	id, ok := s.pathUserID(w, r)
	if !ok {
		return
	}
	ids, err := s.sys.SetUserDisabled(ctx, id, false, s.now())
	if isNotFound(err) {
		s.notFound(w, r)
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
	d, err := s.adminDeleteView(r, a, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "admin_delete", d)
}

// adminDelete deletes a user the way the operator panel does: disable first
// (sessions gone, workspaces suspended), stop the runners, then delete the
// rows. A failed delete leaves the user disabled rather than half alive.
func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	extendDeadline(w)
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
	d, err := s.adminDeleteView(r, a, u)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "admin_delete", d)
	}
	if !s.checkCurrent(r.Context(), w, a, r.FormValue("current"), fail) {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	ids, err := s.sys.SetUserDisabled(ctx, u.ID, true, s.now())
	switch {
	case isNotFound(err):
		s.notFound(w, r)
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
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	deleted, err := s.sys.DeleteAccount(ctx, u.ID)
	if err != nil {
		s.logger.Error("operator delete failed", "event", "operator_action", "action", "delete", "result", "failed",
			"err", err, "user_id", a.User.ID, "target_user_id", u.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
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
		s.notFound(w, r)
		return
	}
	u, ok := s.adminTarget(w, r)
	if !ok {
		return
	}
	if u.VerifiedAt != nil || u.DisabledAt != nil {
		s.notFound(w, r)
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

// extendDeadline lets a slow ForgetWorkspace wait finish before the server's
// write timeout can cut the response. Unsupported writers are left alone.
func extendDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(4 * time.Minute))
}

// DeleteURL is the confirmation page of user id, with the kept state.
func (k adminKeep) DeleteURL(id int64) string {
	u := k.URL(k.Page)
	path := "/admin/users/" + strconv.FormatInt(id, 10) + "/delete"
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return path + u[i:]
	}
	return path
}

func (s *Server) pathUserID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return 0, false
	}
	return id, true
}

func isNotFound(err error) bool    { return errors.Is(err, store.ErrNotFound) }
func isOperatorErr(err error) bool { return errors.Is(err, store.ErrOperator) }

type adminInvitesData struct {
	base
	Invites []store.Invite
	Email   string // the address typed into a refused create form
	Link    string // one-time invite link; set only on the response that creates it
}

func (s *Server) adminInvitesView(r *http.Request, a *authCtx) (adminInvitesData, error) {
	list, err := s.sys.ListInvites(r.Context(), s.now())
	if err != nil {
		return adminInvitesData{}, err
	}
	return adminInvitesData{base: s.base(r, a, "admin", "Invites"), Invites: list}, nil
}

func (s *Server) adminInvites(w http.ResponseWriter, r *http.Request, a *authCtx) {
	d, err := s.adminInvitesView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "admin_invites", d)
}

// adminInviteCreate makes an invite, optionally bound to an email. The link
// is shown once on this response; it is never put in a flash, a URL or a log.
func (s *Server) adminInviteCreate(w http.ResponseWriter, r *http.Request, a *authCtx) {
	email := strings.TrimSpace(r.FormValue("email"))
	if len(email) > adminFieldMax {
		email = email[:adminFieldMax]
	}
	email = strings.ToValidUTF8(email, "")
	var raw string
	var badEmail bool
	if email != "" {
		_, err := store.NormalizeEmail(email)
		badEmail = err != nil
	}
	if !badEmail {
		var err error
		if raw, err = s.sys.CreateInvite(context.WithoutCancel(r.Context()), email, inviteTTL, s.now()); err != nil {
			s.fail(w, err)
			return
		}
	}
	d, err := s.adminInvitesView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	if badEmail {
		d.Email, d.Error = email, msgBadEmail
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "admin_invites", d)
		return
	}
	d.NoRefresh = true
	d.Link = s.cfg.LinkBase() + "/signup?invite=" + raw
	s.operatorAction(r, a, "invite_create", 0)
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "admin_invites", d)
}

func (s *Server) adminInviteRevoke(w http.ResponseWriter, r *http.Request, a *authCtx) {
	err := s.sys.RevokeInvite(context.WithoutCancel(r.Context()), r.FormValue("id"))
	switch {
	case isNotFound(err):
		redirect(w, r, "/admin/invites", msgInviteNotFound)
	case err != nil:
		s.fail(w, err)
	default:
		s.operatorAction(r, a, "invite_revoke", 0)
		redirect(w, r, "/admin/invites", msgInviteRevoked)
	}
}
