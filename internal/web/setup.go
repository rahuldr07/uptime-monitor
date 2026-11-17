package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	setupWindow      = 15 * time.Minute
	setupMaxAttempts = 5
	maxWorkspaceName = 60

	msgSetupLink    = "Open the setup link printed in the server log."
	msgBadEmail     = "Enter a valid email address."
	msgMismatch     = "The passwords do not match."
	msgWorkspaceLen = "The workspace name must be 1 to 60 characters, without control characters."
)

type setupData struct {
	base
	Token         string // a setup token that passed PeekToken, or the one being retried
	AskPassword   bool   // upgraded install: unlock with admin_password
	Email         string
	WorkspaceName string
	Hint          string // shown instead of the form
}

// Form reports whether the page offers the setup form at all.
func (d setupData) Form() bool { return d.Token != "" || d.AskPassword }

// validWorkspaceName reports whether name is 1 to 60 characters with no
// control or format characters.
func validWorkspaceName(name string) bool {
	if n := utf8.RuneCountInString(name); n < 1 || n > maxWorkspaceName {
		return false
	}
	for _, r := range name {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func (s *Server) passwordUnlock() bool { return s.upgraded && s.cfg.AdminPassword != "" }

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	d := setupData{base: s.page(r, "Set up")}
	ws, err := s.sys.Workspace(store.OperatorWorkspaceID).Info(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	d.WorkspaceName = ws.Name
	if tok := r.URL.Query().Get("token"); tok != "" {
		s.logger.Warn("setup attempt", "event", "setup_attempt", "ip", clientIP(r, s.cfg.ProxyHops()))
		if _, err := s.sys.PeekToken(r.Context(), store.TokenSetup, tok, s.now()); err == nil {
			d.Token = tok
		} else if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, err)
			return
		}
	}
	if d.Token == "" {
		d.AskPassword = s.passwordUnlock()
	}
	if !d.Form() {
		d.Hint = msgSetupLink
	}
	s.render(w, r, "setup", d)
}

func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	ip := clientIP(r, s.cfg.ProxyHops())
	tok := r.FormValue("token")
	if ok, _ := s.setupLimiter.Take(clientKey(r, s.cfg.ProxyHops())); !ok {
		s.logger.Warn("setup attempt", "event", "setup_attempt", "ip", ip, "result", "rate_limited")
		d := setupData{base: s.page(r, "Set up"), Token: tok, AskPassword: tok == "" && s.passwordUnlock()}
		d.Error = msgTooMany
		if !d.Form() {
			d.Hint = msgSetupLink
		}
		s.renderStatus(w, r, http.StatusTooManyRequests, "setup", d)
		return
	}
	s.logger.Warn("setup attempt", "event", "setup_attempt", "ip", ip)

	d := setupData{base: s.page(r, "Set up"), Token: tok, AskPassword: tok == "" && s.passwordUnlock(),
		Email: strings.TrimSpace(r.FormValue("email")), WorkspaceName: strings.TrimSpace(r.FormValue("workspace"))}
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
		if !d.Form() {
			d.Hint = msgSetupLink
		}
		s.renderStatus(w, r, http.StatusUnprocessableEntity, "setup", d)
		return
	}

	in := store.SetupInput{Email: email, WorkspaceName: d.WorkspaceName, Now: s.now()}
	if tok != "" {
		if _, err := s.sys.PeekToken(ctx, store.TokenSetup, tok, s.now()); err == nil {
			in.SetupToken = tok
		} else if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, err)
			return
		}
	}
	if in.SetupToken == "" && s.passwordUnlock() && passwordEqual(r.FormValue("admin_password"), s.cfg.AdminPassword) {
		in.PasswordUnlocked = true
	}
	if in.SetupToken == "" && !in.PasswordUnlocked {
		s.notFound(w, r)
		return
	}

	phc, err := s.hasher.Hash(ctx, pw)
	if errors.Is(err, password.ErrBusy) {
		d.Error = msgBusy
		s.renderStatus(w, r, http.StatusServiceUnavailable, "setup", d)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	in.PasswordHash = phc
	u, err := s.sys.CompleteSetup(ctx, in)
	if errors.Is(err, store.ErrSetupDone) {
		s.setupMode.Store(false)
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrSetupDone) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setupMode.Store(false)
	// Seeding and the session must survive a client that disconnects now.
	ctx = context.WithoutCancel(ctx)
	if s.afterSetup != nil {
		if err := s.afterSetup(ctx); err != nil {
			s.logger.Error("setup: seeding failed", "err", err)
		}
	}
	s.logger.Warn("setup complete", "event", "setup_complete", "ip", ip, "user_id", u.ID)
	raw, err := s.sys.CreateSession(ctx, u.ID, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, r, raw)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
