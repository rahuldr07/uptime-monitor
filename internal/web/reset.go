package web

import (
	"context"
	"errors"
	"net/http"

	"github.com/rahuldr07/uptime-monitor/internal/password"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	msgBadLink               = "This link is invalid or has expired."
	msgPasswordChangedSignIn = "Password changed. Sign in to continue."
)

type resetData struct {
	base
	Token   string // set only when the token is valid, which shows the form
	Choose  bool   // the account has no password yet: worded "Choose a password"
	Methods string // for a Choose page: the providers the account still signs in with
}

// chooseWording says whether the token's account still has no password, and
// which providers it signs in with. The link is the same either way, so the
// wording follows the account's state now, not the state when the link was
// sent. Anything unknown reads as a reset.
func (s *Server) chooseWording(ctx context.Context, tok string) (methods string, choose bool) {
	t, err := s.sys.PeekToken(ctx, store.TokenReset, tok, s.now())
	if err != nil {
		return "", false
	}
	has, err := s.sys.HasPassword(ctx, t.UserID)
	if err != nil || has {
		return "", false
	}
	ids, _ := s.myIdentities(ctx, t.UserID)
	return signInLabel(ids), true
}

// resetPage only looks at the token: link scanners and prefetching must not
// use it up.
func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	d := resetData{base: s.page(r, "Reset password")}
	tok := r.URL.Query().Get("token")
	if _, err := s.sys.PeekToken(r.Context(), store.TokenReset, tok, s.now()); err == nil {
		d.Token = tok
		if d.Methods, d.Choose = s.chooseWording(r.Context(), tok); d.Choose {
			d.Title = "Choose a password"
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	} else {
		d.Error = msgBadLink
	}
	s.render(w, r, "reset", d)
}

func (s *Server) resetPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	tok, pw := r.FormValue("token"), r.FormValue("password")
	d := resetData{base: s.page(r, "Reset password"), Token: tok}
	fail := func(code int, msg string) {
		d.Error = msg
		if d.Methods, d.Choose = s.chooseWording(ctx, tok); d.Choose {
			d.Title = "Choose a password"
		}
		s.renderStatus(w, r, code, "reset", d)
	}
	// Input checks come first, so a bad form never reaches the token lookup.
	if err := password.CheckLength(pw); err != nil {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if pw != r.FormValue("confirm") {
		fail(http.StatusUnprocessableEntity, msgMismatch)
		return
	}
	if _, err := s.sys.PeekToken(ctx, store.TokenReset, tok, s.now()); errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	phc, err := s.hasher.Hash(ctx, pw)
	if errors.Is(err, password.ErrBusy) {
		fail(http.StatusServiceUnavailable, msgBusy)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	cur, signedIn := s.currentAuth(r)
	u, err := s.sys.ResetPassword(ctx, tok, phc, s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Warn("password reset", "event", "password_reset", "user_id", u.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	s.noticePasswordChanged(u.Email)
	// Someone else's browser session is left alone, as in verifyPost.
	if signedIn && cur.User.ID != u.ID {
		d.Token, d.Flash = "", msgPasswordChangedSignIn
		s.render(w, r, "reset", d)
		return
	}
	// The link never signs in an account that has two-factor on.
	if u.TOTPEnabledAt != nil {
		redirect(w, r, "/login", msgPasswordChangedSignIn)
		return
	}
	s.startSession(w, r, u, "reset", "/", false)
}
