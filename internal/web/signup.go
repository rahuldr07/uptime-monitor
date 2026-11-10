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
	if raw == "" && s.cfg.Signup != "invite" {
		http.NotFound(w, r)
		return
	}
	d := signupData{base: s.page(r, "Sign up"), Invite: raw,
		Email: strings.TrimSpace(r.FormValue("email")), WorkspaceName: strings.TrimSpace(r.FormValue("workspace"))}
	fail := func(code int, msg string) {
		d.Error = msg
		s.renderStatus(w, r, code, "signup", d)
	}

	// 1. Address limit. It is refunded unless the form was a real attempt
	// with a valid invite, so probing without one costs nothing.
	if ok, _ := s.signupByAddr.Take(addr); !ok {
		s.logger.Warn("sign-up throttled", "event", "signup_throttled", "ip", ip)
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	refundAddr, refundGlobal := true, false
	defer func() {
		if refundAddr {
			s.signupByAddr.Undo(addr)
		}
		if refundGlobal {
			s.signupGlobal.Undo("all")
		}
	}()

	// 2. Validation. Its result is shown only for a valid invite, so a
	// duplicate email never answers anyone without one.
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

	// 3. Invite, peeked once.
	tok, err := s.sys.PeekToken(ctx, store.TokenInvite, raw, s.now())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	d.EmailLocked = tok.Email != ""
	if d.Error != "" {
		refundAddr = false
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "signup", d)
		return
	}

	// 4. Instance-wide limit, spent only by a valid invite.
	if ok, _ := s.signupGlobal.Take("all"); !ok {
		s.logger.Warn("sign-up throttled", "event", "signup_throttled", "ip", ip, "scope", "instance")
		fail(http.StatusTooManyRequests, msgTooMany)
		return
	}
	refundGlobal = true
	refundAddr = false

	// 5. Hash.
	phc, err := s.hasher.Hash(ctx, pw)
	if errors.Is(err, password.ErrBusy) {
		refundAddr = true
		fail(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}

	// 6. Create the account; the workspace cap is checked in the transaction.
	// A failure leaves the invite unused.
	u, _, err := s.sys.CreateAccount(ctx, store.AccountInput{Email: email, PasswordHash: phc,
		WorkspaceName: d.WorkspaceName, Verified: true, InviteToken: raw,
		MaxWorkspaces: *s.cfg.Limits.MaxWorkspaces, Now: s.now()})
	switch {
	case errors.Is(err, store.ErrWorkspaceCap):
		refundGlobal = false
		fail(http.StatusForbidden, msgNoNewAccounts)
		return
	case errors.Is(err, store.ErrEmailTaken):
		refundGlobal = false
		s.logger.Warn("sign-up duplicate", "event", "signup_duplicate", "ip", ip)
		fail(http.StatusUnprocessableEntity, msgEmailTaken)
		return
	case errors.Is(err, store.ErrInviteEmail):
		refundGlobal = false
		fail(http.StatusUnprocessableEntity, msgInviteEmail)
		return
	case errors.Is(err, store.ErrNotFound):
		refundAddr = true
		http.NotFound(w, r)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	refundGlobal = false

	// 7. Success. The session must survive a client that disconnects now.
	ctx = context.WithoutCancel(ctx)
	s.logger.Info("sign-up", "event", "signup", "user_id", u.ID, "ip", ip)
	sess, err := s.sys.CreateSession(ctx, u.ID, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, r, sess)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
