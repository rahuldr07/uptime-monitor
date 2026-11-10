package web

import (
	"errors"
	"net/http"

	"uptimemon/internal/password"
	"uptimemon/internal/store"
)

const msgBadLink = "This link is invalid or has expired."

type resetData struct {
	base
	Token string // set only when the token is valid, which shows the form
}

// resetPage only looks at the token: link scanners and prefetching must not
// use it up.
func (s *Server) resetPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	d := resetData{base: s.page(r, "Reset password · Uptime")}
	tok := r.URL.Query().Get("token")
	if _, err := s.sys.PeekToken(r.Context(), store.TokenReset, tok, s.now()); err == nil {
		d.Token = tok
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
	d := resetData{base: s.page(r, "Reset password · Uptime"), Token: tok}
	fail := func(code int, msg string) {
		d.Error = msg
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
		http.NotFound(w, r)
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
	u, err := s.sys.ResetPassword(ctx, tok, phc, s.now())
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	raw, err := s.sys.CreateSession(ctx, u.ID, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookie(w, r, raw)
	s.logger.Warn("password reset", "event", "password_reset", "user_id", u.ID, "ip", clientKey(r, s.cfg.ProxyHops()))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
