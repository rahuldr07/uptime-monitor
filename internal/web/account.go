package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/mail"
	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	msgSignInAgain     = "Sign in again to continue."
	msgBadCurrent      = "Current password is incorrect."
	msgPasswordChanged = "Password changed. Other sessions were signed out."
	msgSignedOutAll    = "Signed out everywhere."
	msgLastOperator    = "The last operator can't delete their account."
	msgAccountDeleted  = "Your account was deleted."

	msgPwLinkSent     = "Link sent."
	msgPwLinkTooMany  = "A link was sent recently. Check your inbox or try again later."
	msgPwLinkFailed   = "We couldn't send the email. Try again later."
	msgPwLinkUseEmail = "To set a password, use Set a password to get a link by email."
	pwLinkPerHour     = 3

	passwordCheckWindow = 15 * time.Minute
	passwordCheckMax    = 5
)

// recentAuthWindow is how fresh auth_at must be for a social-only account's
// sensitive actions.
const recentAuthWindow = 10 * time.Minute

// recentAuth reports whether the session authenticated for real within the window.
func (s *Server) recentAuth(a *authCtx) bool {
	if a.AuthAt == nil {
		return false
	}
	d := s.now().Sub(*a.AuthAt)
	return d >= -time.Minute && d <= recentAuthWindow
}

// recentVerified reports whether the session was verified recently enough for
// a sensitive action. For an account with two-factor on that means a valid
// code within the window (mfa_at); a password or provider sign-in (auth_at)
// does not count. Other accounts follow recentAuth.
func (s *Server) recentVerified(a *authCtx) bool {
	if !a.twoFactor() {
		return s.recentAuth(a)
	}
	if a.MFAAt == nil {
		return false
	}
	d := s.now().Sub(*a.MFAAt)
	return d >= -time.Minute && d <= recentAuthWindow
}

func providerLabel(name string) string {
	if name == "github" {
		return "GitHub"
	}
	return "Google"
}

func (s *Server) myIdentities(ctx context.Context, userID int64) ([]store.Identity, error) {
	return s.sys.Identities(ctx, userID)
}

// reauthState says whether a sensitive action needs a fresh sign-in (the
// account has no password and the session is not recent), and the links that
// provide one.
func (s *Server) reauthState(a *authCtx, ids []store.Identity, next string) (bool, []oauthLink) {
	if a.HasPassword || s.recentAuth(a) {
		return false, nil
	}
	var links []oauthLink
	for _, i := range ids {
		if s.oauth[i.Provider] != nil {
			links = append(links, oauthLink{Label: providerLabel(i.Provider), Name: i.Provider, Href: "/auth/" + i.Provider + "/start?" +
				url.Values{"intent": {"reauth"}, "next": {next}}.Encode()})
		}
	}
	return true, links
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ctx := r.Context()
	d, err := s.settingsView(r, a, secSecurity)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.setError(msg)
		s.renderStatus(w, r, code, "settings", d)
	}
	// With mail, the first password is chosen through the emailed link: the
	// inbox, not a recent Google or GitHub sign-in, is the proof.
	if !a.HasPassword && s.mail != nil {
		fail(http.StatusConflict, msgPwLinkUseEmail)
		return
	}
	pw := r.FormValue("new")
	if err := password.CheckLength(pw); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if pw != r.FormValue("confirm") {
		fail(http.StatusUnprocessableEntity, msgMismatch)
		return
	}
	if !s.checkSensitive(w, r, a, fail) {
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
	if a.HasPassword {
		err = s.sys.SetPassword(ctx, a.User.ID, newPHC, a.sessionHash)
	} else {
		// checkCurrent has just required a recent sign-in.
		err = s.sys.SetFirstPassword(ctx, a.User.ID, newPHC, a.sessionHash)
	}
	if errors.Is(err, store.ErrPasswordSet) {
		fail(http.StatusConflict, "This account already has a password. Reload the page.")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Info("password changed", "event", "password_changed", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	s.noticePasswordChanged(a.User.Email)
	redirect(w, r, "/settings/security", msgPasswordChanged)
}

// accountPasswordLink emails the account address a reset link worded for
// choosing a first password. It is the same token and page as /forgot; the
// token is created inside compose so a dropped message never replaces a link.
// The address is not put in the URL: the page shows a persistent "check your
// email" state for ?sent=1 instead.
func (s *Server) accountPasswordLink(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if s.mail == nil {
		s.notFound(w, r)
		return
	}
	d, err := s.settingsView(r, a, secSecurity)
	if err != nil {
		s.fail(w, err)
		return
	}
	if a.HasPassword {
		d.Error = "This account already has a password. Reload the page."
		s.renderStatus(w, r, http.StatusConflict, "settings", d)
		return
	}
	// The shared per-address budget is checked first, so a refusal there does
	// not use up the user's own 3 an hour; a slot is given back when nothing
	// was sent.
	if ok, _ := s.mailByAddr.Take(clientKey(r, s.cfg.ProxyHops())); !ok {
		s.logger.Warn("set-password link throttled", "event", "mail_throttled", "ip", clientIP(r, s.cfg.ProxyHops()))
		redirect(w, r, "/settings/security", msgPwLinkTooMany)
		return
	}
	key := strconv.FormatInt(a.User.ID, 10)
	if ok, _ := s.pwLinkByUser.Take(key); !ok {
		redirect(w, r, "/settings/security", msgPwLinkTooMany)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	err = s.mail.Send(a.User.Email, func() (string, string, error) {
		raw, err := s.sys.CreateToken(ctx, store.TokenReset, a.User.ID, resetTTL, s.now())
		if err != nil {
			return "", "", err
		}
		subject, body := setPasswordMail(s.cfg.BaseURL + "/reset?token=" + raw)
		return subject, body, nil
	})
	if errors.Is(err, mail.ErrDropped) {
		s.pwLinkByUser.Undo(key)
		redirect(w, r, "/settings/security", msgPwLinkTooMany)
		return
	}
	if err != nil {
		s.pwLinkByUser.Undo(key)
		s.logger.Error("set password: cannot send link", "err", err, "user_id", a.User.ID)
		d.Error = msgPwLinkFailed
		s.renderStatus(w, r, http.StatusBadGateway, "settings", d)
		return
	}
	s.logger.Info("set-password link sent", "event", "password_link_sent", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	redirect(w, r, "/settings/security?sent=1", msgPwLinkSent)
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

// checkCurrent is the password check of an action that two-factor does not
// guard (the operator's confirmation before deleting a user).
func (s *Server) checkCurrent(ctx context.Context, w http.ResponseWriter, a *authCtx, cur string, fail func(int, string)) bool {
	return s.checkPassword(ctx, w, a, cur, fail, false)
}

// checkSensitive guards the sensitive actions of spec §9. Password accounts
// type the current password; accounts with two-factor on also need a code
// unless one verified this session in the last ten minutes, and for them a
// recent provider sign-in no longer stands in for a password.
func (s *Server) checkSensitive(w http.ResponseWriter, r *http.Request, a *authCtx, fail func(int, string)) bool {
	if !s.checkPassword(r.Context(), w, a, r.FormValue("current"), fail, a.twoFactor()) {
		return false
	}
	return s.checkFresh(r, a, fail, false)
}

// isRecoveryInput says whether a code field holds something other than a
// 6-digit code, which is then tried as a recovery code.
func isRecoveryInput(code string) bool {
	_, six := sixDigits(code)
	return !six
}

// checkFresh requires a valid code for an account with two-factor on, unless
// the session is already verified (and always when always is set). A good
// code marks the session verified. It has answered when it reports false.
func (s *Server) checkFresh(r *http.Request, a *authCtx, fail func(int, string), always bool) bool {
	if !a.twoFactor() || (!always && s.recentVerified(a)) {
		return true
	}
	code := strings.TrimSpace(r.FormValue("code"))
	if code == "" {
		fail(http.StatusUnprocessableEntity, msgCodeRequired)
		return false
	}
	ctx := context.WithoutCancel(r.Context())
	if ok, msg := s.checkCode(ctx, a.User, code, isRecoveryInput(code), ""); !ok {
		fail(codeStatus(msg), msg)
		return false
	}
	now := s.now()
	if err := s.sys.MarkMFA(ctx, a.sessionHash, now); err != nil {
		s.logger.Error("two-factor: cannot mark the session", "err", err, "user_id", a.User.ID)
		fail(http.StatusInternalServerError, msgMFAError)
		return false
	}
	a.MFAAt = &now
	return true
}

// checkPassword verifies the signed-in user's current password with the
// per-user limiter. On failure it has already answered and reports false. A
// social-only account has no password: unless skipRecent is set, a recent
// sign-in stands in for it.
func (s *Server) checkPassword(ctx context.Context, w http.ResponseWriter, a *authCtx, cur string, fail func(int, string), skipRecent bool) bool {
	phc, err := s.sys.PasswordHash(ctx, a.User.ID)
	if err != nil {
		s.fail(w, err)
		return false
	}
	// A social-only account has no password to check. The empty hash is never
	// verified; a sign-in within the last 10 minutes stands in for it, and
	// otherwise the user must sign in again with a provider (§7).
	if phc == "" {
		if skipRecent || s.recentAuth(a) {
			return true
		}
		fail(http.StatusForbidden, msgSignInAgain)
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
	d, err := s.settingsView(r, a, secDanger)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.setError(msg)
		s.renderStatus(w, r, code, "settings", d)
	}
	if !s.checkSensitive(w, r, a, fail) {
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

const msgLastMethod = "Add a password or connect another provider first."

// accountIdentityDelete disconnects a provider. An account with a password
// confirms with it; one without needs a recent sign-in. The store refuses to
// remove the last sign-in method.
func (s *Server) accountIdentityDelete(w http.ResponseWriter, r *http.Request, a *authCtx) {
	name := r.PathValue("provider")
	if name != "google" && name != "github" {
		s.notFound(w, r)
		return
	}
	d, err := s.settingsView(r, a, secSecurity)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.setError(msg)
		s.renderStatus(w, r, code, "settings", d)
	}
	if !s.checkSensitive(w, r, a, fail) {
		return
	}
	err = s.sys.UnlinkIdentity(r.Context(), a.User.ID, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.notFound(w, r)
		return
	case errors.Is(err, store.ErrLastSignInMethod):
		fail(http.StatusUnprocessableEntity, msgLastMethod)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	s.logger.Info("oauth unlinked", "event", "oauth_unlink", "provider", name, "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	s.noticeMethodChanged(a.User, providerLabel(name), false)
	redirect(w, r, "/settings/security", providerLabel(name)+" disconnected.")
}

// accountIdentityConnect lets a password account that has been signed in a
// while confirm its password, which counts as a fresh sign-in, before
// connecting a provider. A social-only account needs a recent sign-in as is.
func (s *Server) accountIdentityConnect(w http.ResponseWriter, r *http.Request, a *authCtx) {
	name := r.PathValue("provider")
	if s.oauth[name] == nil {
		s.notFound(w, r)
		return
	}
	d, err := s.settingsView(r, a, secSecurity)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.setError(msg)
		s.renderStatus(w, r, code, "settings", d)
	}
	if !s.checkSensitive(w, r, a, fail) {
		return
	}
	if a.HasPassword && !a.twoFactor() {
		if err := s.sys.MarkReauth(r.Context(), a.sessionHash, s.now()); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/auth/"+name+"/start?intent=link", http.StatusSeeOther)
}

// restartRunners reloads every target of ws, after a failed delete or an enable.
func (s *Server) restartRunners(ctx context.Context, ws *store.WS) {
	// The request context may be the reason the delete failed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	ts, err := ws.ListTargets(ctx)
	if err != nil {
		s.logger.Error("restart runners", "workspace", ws.ID(), "err", err)
		return
	}
	for _, t := range ts {
		if err := s.mon.Reload(ctx, t.ID); err != nil {
			s.logger.Error("restart runner", "target", t.ID, "err", err)
		}
	}
}
