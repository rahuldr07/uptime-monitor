package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	msgForgotSent = "If an account exists for that address, we sent a reset link."

	resetTTL = time.Hour
)

type forgotData struct {
	base
	Sent bool
}

func (s *Server) forgotPage(w http.ResponseWriter, r *http.Request) {
	if s.mail == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, "forgot", forgotData{base: s.page(r, "Forgot password")})
}

// forgotPost answers the same for every address, so it cannot tell which
// accounts exist. The token is created inside compose, so a dropped message
// never replaces an earlier link.
func (s *Server) forgotPost(w http.ResponseWriter, r *http.Request) {
	if s.mail == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	d := forgotData{base: s.page(r, "Forgot password")}
	if ok, _ := s.mailByAddr.Take(clientKey(r, s.cfg.ProxyHops())); !ok {
		s.logger.Warn("forgot throttled", "event", "mail_throttled", "ip", clientIP(r, s.cfg.ProxyHops()))
		d.Error = msgTooMany
		s.renderStatus(w, r, http.StatusTooManyRequests, "forgot", d)
		return
	}
	u, _, err := s.sys.UserByEmail(r.Context(), r.FormValue("email"))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	if err == nil && u.DisabledAt == nil {
		ctx := context.WithoutCancel(r.Context())
		if err := s.mail.Send(u.Email, func() (string, string, error) {
			raw, err := s.sys.CreateToken(ctx, store.TokenReset, u.ID, resetTTL, s.now())
			if err != nil {
				return "", "", err
			}
			subject, body := resetMail(s.cfg.BaseURL + "/reset?token=" + raw)
			return subject, body, nil
		}); err != nil {
			s.logger.Error("forgot: cannot send reset link", "err", err, "user_id", u.ID)
		}
	}
	d.Flash, d.Sent = msgForgotSent, true
	s.render(w, r, "forgot", d)
}

// noticePasswordChanged tells the owner of email that the password changed.
func (s *Server) noticePasswordChanged(email string) {
	if s.mail == nil {
		return
	}
	subject, body := passwordChangedMail(s.cfg.BaseURL)
	s.mail.Notice(email, subject, body)
}
