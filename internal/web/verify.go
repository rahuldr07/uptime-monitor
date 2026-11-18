package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/mail"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	msgCheckEmail     = "Check your email to finish signing up."
	msgResendSent     = "If an unverified account exists for that address, we sent a new link."
	msgUnverified     = "Verify your email to sign in."
	msgVerifiedSignIn = "Email verified. Sign in to continue."

	verifyTTL = 24 * time.Hour
)

type verifyData struct {
	base
	Token  string // set only when the token is valid, which shows the confirm button
	Resend bool   // show the resend form
}

// sendVerify mails u a new verification link. The token is created inside
// compose, so a dropped message never replaces an earlier link, and on a
// context that survives a client disconnect.
func (s *Server) sendVerify(ctx context.Context, u store.User) error {
	ctx = context.WithoutCancel(ctx)
	// A message the budgets refused is not an error here: callers answer the
	// same either way, as before.
	err := s.mail.Send(u.Email, func() (string, string, error) {
		raw, err := s.sys.CreateToken(ctx, store.TokenVerify, u.ID, verifyTTL, s.now())
		if err != nil {
			return "", "", err
		}
		subject, body := verifyMail(s.cfg.BaseURL + "/verify?token=" + raw)
		return subject, body, nil
	})
	if errors.Is(err, mail.ErrDropped) {
		return nil
	}
	return err
}

func (s *Server) verifyBadLink(w http.ResponseWriter, r *http.Request) {
	d := verifyData{base: s.page(r, "Confirm your email"), Resend: s.mail != nil}
	d.Error = msgBadLink
	s.renderStatus(w, r, http.StatusNotFound, "verify", d)
}

// verifyPage only looks at the token: link scanners and prefetching must not
// use it up.
func (s *Server) verifyPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	tok := r.URL.Query().Get("token")
	_, err := s.sys.PeekToken(r.Context(), store.TokenVerify, tok, s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.verifyBadLink(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, r, "verify", verifyData{base: s.page(r, "Confirm your email"), Token: tok})
}

func (s *Server) verifyPost(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()
	tok := r.FormValue("token")
	if _, err := s.sys.PeekToken(ctx, store.TokenVerify, tok, s.now()); errors.Is(err, store.ErrNotFound) {
		s.verifyBadLink(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	u, err := s.sys.VerifyEmail(ctx, tok, s.now())
	if errors.Is(err, store.ErrNotFound) {
		s.verifyBadLink(w, r)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Info("email verified", "event", "email_verified", "user_id", u.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	ctx = context.WithoutCancel(ctx)
	// Someone else's browser session is left alone: the link confirms the
	// address but never signs anyone in over an existing session.
	if a, ok := s.currentAuth(r); ok && a.User.ID != u.ID {
		d := verifyData{base: s.page(r, "Confirm your email")}
		d.Flash = msgVerifiedSignIn
		s.render(w, r, "verify", d)
		return
	}
	// The link never signs in an account that has two-factor on.
	if u.TOTPEnabledAt != nil {
		redirect(w, r, "/login", msgVerifiedSignIn)
		return
	}
	s.startSession(w, r, u, "verify", "/", false)
}

// verifyResend answers the same for every address, so it cannot tell which
// accounts exist.
func (s *Server) verifyResend(w http.ResponseWriter, r *http.Request) {
	if s.mail == nil {
		s.notFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	d := verifyData{base: s.page(r, "Confirm your email")}
	if ok, _ := s.mailByAddr.Take(clientKey(r, s.cfg.ProxyHops())); !ok {
		s.logger.Warn("resend throttled", "event", "mail_throttled", "ip", clientIP(r, s.cfg.ProxyHops()))
		d.Error, d.Resend = msgTooMany, true
		s.renderStatus(w, r, http.StatusTooManyRequests, "verify", d)
		return
	}
	u, _, err := s.sys.UserByEmail(r.Context(), r.FormValue("email"))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if err == nil && u.VerifiedAt == nil && u.DisabledAt == nil {
		if err := s.sendVerify(r.Context(), u); err != nil {
			s.logger.Error("resend: cannot send verification", "err", err, "user_id", u.ID)
		}
	}
	d.Flash = msgResendSent
	s.render(w, r, "verify", d)
}

// errNoticeThrottled is returned from compose when the owner already got a
// notice today; ownerNotice ignores it.
var errNoticeThrottled = errors.New("duplicate notice already sent today")

// ownerNotice sends the duplicate sign-up notice to the account that owns
// email, at most once a day, and returns that account's ID (0 if none). The
// daily slot is taken inside compose, so a dropped message does not use it.
func (s *Server) ownerNotice(ctx context.Context, email string) int64 {
	u, _, err := s.sys.UserByEmail(ctx, email)
	if err != nil || u.DisabledAt != nil {
		return 0
	}
	err = s.mail.Send(u.Email, func() (string, string, error) {
		if ok, _ := s.dupNotice.Take(strconv.FormatInt(u.ID, 10)); !ok {
			return "", "", errNoticeThrottled
		}
		subject, body := duplicateMail(s.cfg.BaseURL)
		return subject, body, nil
	})
	if err != nil && !errors.Is(err, errNoticeThrottled) && !errors.Is(err, mail.ErrDropped) {
		s.logger.Error("sign-up: cannot send notice", "err", err, "user_id", u.ID)
	}
	return u.ID
}
