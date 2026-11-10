package web

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"uptimemon/internal/password"
	"uptimemon/internal/store"
)

const (
	signupPerAddrHour = 5

	msgInviteOnly    = "Sign-up on this instance is by invitation. Ask the operator for a link."
	msgBadInvite     = "This invitation is invalid or has expired."
	msgNoNewAccounts = "This instance is not accepting new accounts."
	msgEmailTaken    = "An account with this email already exists. Sign in instead."
	msgInviteEmail   = "This invitation is for a different email address."
)

type signupData struct {
	base
	Invite        string // an invite that passed PeekToken, or the one being retried
	Email         string
	EmailLocked   bool // the invite is bound to Email
	WorkspaceName string
	Hint          string // shown instead of the form
}

func (s *Server) signupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	d := signupData{base: s.page(r, "Sign up")}
	raw := r.URL.Query().Get("invite")
	if raw == "" {
		if s.cfg.Signup != "invite" {
			http.NotFound(w, r)
			return
		}
		d.Hint = msgInviteOnly
		s.render(w, r, "signup", d)
		return
	}
	t, err := s.sys.PeekToken(r.Context(), store.TokenInvite, raw, s.now())
	switch {
	case err == nil:
		d.Invite, d.Email, d.EmailLocked = raw, t.Email, t.Email != ""
	case errors.Is(err, store.ErrNotFound):
		d.Hint = msgBadInvite
	default:
		s.fail(w, err)
		return
	}
	s.render(w, r, "signup", d)
}

func (s *Server) signupPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	ip := clientIP(r, s.cfg.ProxyHops())
	addr := clientKey(r, s.cfg.ProxyHops())
	raw := r.FormValue("invite")
	d := signupData{base: s.page(r, "Sign up"), Invite: raw,
		Email: strings.TrimSpace(r.FormValue("email")), WorkspaceName: strings.TrimSpace(r.FormValue("workspace"))}
	if t, err := s.sys.PeekToken(ctx, store.TokenInvite, raw, s.now()); err == nil && t.Email != "" {
		d.EmailLocked = true
	}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "signup", d)
	}

	// 1. Limits. The slots are refunded when no account was created for a
	// reason other than the form (unknown invite, busy hasher); validation
	// failures keep theirs.
	if ok, _ := s.signupByAddr.Take(addr); !ok {
		s.logger.Warn("sign-up throttled", "event", "signup_throttled", "ip", ip)
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	if ok, _ := s.signupGlobal.Take("all"); !ok {
		s.signupByAddr.Undo(addr)
		s.logger.Warn("sign-up throttled", "event", "signup_throttled", "ip", ip, "scope", "instance")
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	refund := true
	defer func() {
		if refund {
			s.signupByAddr.Undo(addr)
			s.signupGlobal.Undo("all")
		}
	}()

	// 2. Validation.
	pw := r.FormValue("password")
	email, emailErr := store.NormalizeEmail(d.Email)
	pwErr := password.CheckLength(pw)
	switch {
	case emailErr != nil:
		d.Error = msgBadEmail
	case pwErr != nil:
		d.Error = pwErr.Error()
	case pw != r.FormValue("confirm"):
		d.Error = msgMismatch
	case !validWorkspaceName(d.WorkspaceName):
		d.Error = msgWorkspaceLen
	}
	if d.Error != "" {
		refund = false
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "signup", d)
		return
	}

	// 3. Invite.
	if _, err := s.sys.PeekToken(ctx, store.TokenInvite, raw, s.now()); errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	refund = false

	// 4. Workspace cap.
	if max := *s.cfg.Limits.MaxWorkspaces; max > 0 {
		n, err := s.sys.CountWorkspaces(ctx)
		if err != nil {
			s.fail(w, err)
			return
		}
		if n >= max {
			fail(http.StatusForbidden, msgNoNewAccounts)
			return
		}
	}

	// 5. Hash.
	phc, err := s.hasher.Hash(ctx, pw)
	if errors.Is(err, password.ErrBusy) {
		refund = true
		fail(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	// 6. Create the account. A failure leaves the invite unused.
	u, _, err := s.sys.CreateAccount(ctx, store.AccountInput{Email: email, PasswordHash: phc,
		WorkspaceName: d.WorkspaceName, Verified: true, InviteToken: raw, Now: s.now()})
	switch {
	case errors.Is(err, store.ErrEmailTaken):
		fail(http.StatusUnprocessableEntity, msgEmailTaken)
		return
	case errors.Is(err, store.ErrInviteEmail):
		fail(http.StatusUnprocessableEntity, msgInviteEmail)
		return
	case errors.Is(err, store.ErrNotFound):
		refund = true
		http.NotFound(w, r)
		return
	case err != nil:
		s.fail(w, err)
		return
	}

	// 7. Success. The session must survive a client that disconnects now.
	ctx = context.WithoutCancel(ctx)
	s.logger.Warn("sign-up", "event", "signup", "user_id", u.ID, "ip", ip)
	sess, err := s.sys.CreateSession(ctx, u.ID, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, r, sess)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
