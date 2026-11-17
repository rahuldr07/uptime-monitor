package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

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

	passwordCheckWindow = 15 * time.Minute
	passwordCheckMax    = 5
)

// recentAuthWindow is how fresh auth_at must be for a social-only account's
// sensitive actions.
const recentAuthWindow = 10 * time.Minute

type accountProvider struct {
	Name, Label string
	Enabled     bool // can be started from this instance
	Connected   bool
	Email       string // the provider's email at link time
}

type accountData struct {
	base
	CanDelete   bool // operators cannot delete their account here
	HasPassword bool
	Providers   []accountProvider
	SignInLabel string      // the connected providers, for "You sign in with …"
	SoleMethod  bool        // the only way to sign in is one provider: it cannot be disconnected
	Recent      bool        // auth_at is within the window
	NeedReauth  bool        // no password and no recent sign-in: sensitive forms are replaced by reauth links
	Reauth      []oauthLink // one per connected, enabled provider
}

// recentAuth reports whether the session authenticated for real within the window.
func (s *Server) recentAuth(a *authCtx) bool {
	if a.AuthAt == nil {
		return false
	}
	d := s.now().Sub(*a.AuthAt)
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

// accountView builds the Account page's view model.
func (s *Server) accountView(r *http.Request, a *authCtx) (accountData, error) {
	ids, err := s.myIdentities(r.Context(), a.User.ID)
	if err != nil {
		return accountData{}, err
	}
	d := accountData{base: s.base(r, a, "account", "Account"), CanDelete: !a.User.IsOperator, HasPassword: a.HasPassword}
	d.Recent = s.recentAuth(a)
	d.NeedReauth, d.Reauth = s.reauthState(a, ids, "/account")
	d.SoleMethod = !a.HasPassword && len(ids) == 1
	for _, name := range []string{"google", "github"} {
		p := accountProvider{Name: name, Label: providerLabel(name), Enabled: s.oauth[name] != nil}
		for _, i := range ids {
			if i.Provider == name {
				p.Connected, p.Email = true, i.Email
			}
		}
		if p.Connected {
			if d.SignInLabel != "" {
				d.SignInLabel += " or "
			}
			d.SignInLabel += p.Label
		}
		if p.Enabled || p.Connected {
			d.Providers = append(d.Providers, p)
		}
	}
	return d, nil
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	d, err := s.accountView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "account", d)
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ctx := r.Context()
	d, err := s.accountView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
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
	// A social-only account has no password to check. The empty hash is never
	// verified; a sign-in within the last 10 minutes stands in for it, and
	// otherwise the user must sign in again with a provider (§7).
	if phc == "" {
		if s.recentAuth(a) {
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
	d, err := s.accountView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
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
	d, err := s.accountView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "account", d)
	}
	if !s.checkCurrent(r.Context(), w, a, r.FormValue("current"), fail) {
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
	s.noticeMethodChanged(a.User.Email, providerLabel(name), false)
	redirect(w, r, "/account", providerLabel(name)+" disconnected.")
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
	d, err := s.accountView(r, a)
	if err != nil {
		s.fail(w, err)
		return
	}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "account", d)
	}
	if !s.checkCurrent(r.Context(), w, a, r.FormValue("current"), fail) {
		return
	}
	if a.HasPassword {
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
