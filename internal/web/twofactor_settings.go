package web

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/rahuldr07/uptime-monitor/internal/qr"
	"github.com/rahuldr07/uptime-monitor/internal/store"
	"github.com/rahuldr07/uptime-monitor/internal/totp"
)

const recoveryCodeCount = 10

// groupSecret writes a base32 secret in groups of four, for typing in by hand.
func groupSecret(s string) string {
	var parts []string
	for len(s) > 4 {
		parts, s = append(parts, s[:4]), s[4:]
	}
	return strings.Join(append(parts, s), " ")
}

// issuer is the name an authenticator app shows: "Uptime (host)", the host of
// the public base URL without its port.
func (s *Server) issuer(r *http.Request) string {
	host := ""
	if u, err := url.Parse(s.cfg.LinkBase()); err == nil {
		host = u.Hostname()
	}
	if host == "" {
		host = strings.Split(r.Host, ":")[0]
	}
	return "Uptime (" + host + ")"
}

// twoFactorView fills the Two-factor card: on (codes left), setup (a pending
// secret, still valid) or off.
func (s *Server) twoFactorView(r *http.Request, a *authCtx, d *settingsData) error {
	ctx := r.Context()
	if a.twoFactor() {
		left, err := s.sys.RecoveryCodesLeft(ctx, a.User.ID)
		if err != nil {
			return err
		}
		d.TFState, d.TFLeft, d.TFLow = tfOn, left, left <= 2
		return nil
	}
	d.TFState = tfOff
	secret, err := s.sys.PendingTOTP(ctx, a.User.ID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	d.TFState, d.TFSecret, d.NoRefresh = tfSetup, groupSecret(secret), true
	// A URI too long for the encoder leaves the typed secret alone.
	if code, err := qr.Encode(totp.URI(s.issuer(r), a.User.Email, secret)); err == nil {
		svg := strings.Replace(code.SVG(4), `aria-label="QR code"`, `aria-label="QR code for your authenticator app"`, 1)
		d.TFQR = template.HTML(svg) // markup built by internal/qr from numbers only
	}
	return nil
}

// settingsFail returns a failure renderer for the Security section.
func (s *Server) settingsFail(w http.ResponseWriter, r *http.Request, a *authCtx) (func(int, string), bool) {
	d, err := s.settingsView(r, a, secSecurity)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	return func(code int, msg string) {
		d.TFError = msg
		s.renderStatus(w, r, code, "settings", d)
	}, true
}

func (s *Server) twoFactorSetup(w http.ResponseWriter, r *http.Request, a *authCtx) {
	fail, ok := s.settingsFail(w, r, a)
	if !ok {
		return
	}
	if a.twoFactor() {
		fail(http.StatusConflict, msgMFAAlreadyOn)
		return
	}
	// Starting setup is a sensitive step: a password account types its
	// current password (every time, however fresh the session); a social-only
	// one needs a recent provider sign-in. Confirming needs only the pending
	// secret, so the 15 minutes of the secret are not cut to the 10 of a
	// recent sign-in.
	if !s.checkPassword(r.Context(), w, a, r.FormValue("current"), fail, false) {
		return
	}
	secret, err := totp.NewSecret()
	if err != nil {
		s.fail(w, err)
		return
	}
	if _, err := s.sys.BeginTOTP(r.Context(), a.User.ID, secret, s.now()); errors.Is(err, store.ErrNotFound) {
		fail(http.StatusConflict, msgMFAAlreadyOn)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/settings/security#two-factor", http.StatusSeeOther)
}

func newRecoveryCodes() (codes, hashes []string, err error) {
	if codes, err = totp.NewRecoveryCodes(recoveryCodeCount); err != nil {
		return nil, nil, err
	}
	for _, c := range codes {
		hashes = append(hashes, totp.HashRecovery(c))
	}
	return codes, hashes, nil
}

// showCodes renders the Security page with the new recovery codes, once.
func (s *Server) showCodes(w http.ResponseWriter, r *http.Request, a *authCtx, codes []string, note string) {
	d, err := s.settingsView(r, a, secSecurity)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.TFState, d.TFCodes, d.TFNote, d.NoRefresh = tfCodes, codes, note, true
	s.render(w, r, "settings", d)
}

func (s *Server) twoFactorConfirm(w http.ResponseWriter, r *http.Request, a *authCtx) {
	fail, ok := s.settingsFail(w, r, a)
	if !ok {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if a.twoFactor() {
		fail(http.StatusConflict, msgMFAAlreadyOn)
		return
	}
	secret, err := s.sys.PendingTOTP(ctx, a.User.ID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		fail(http.StatusConflict, msgSetupExpired)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	// Someone who stole a session must not be able to take the account over
	// with their own authenticator: this step needs a recent sign-in too.
	code := strings.TrimSpace(r.FormValue("code"))
	if code == "" {
		fail(http.StatusUnprocessableEntity, msgCodeRequired)
		return
	}
	// The same gate as every other code check; only the secret differs.
	var step int64
	if ok, msg := s.checkWith(a.User, "", false, func() (bool, error) {
		digits, six := sixDigits(code)
		if !six {
			return false, nil
		}
		var matched bool
		step, matched = totp.Match(secret, digits, s.now())
		return matched, nil
	}); !ok {
		fail(codeStatus(msg), msg)
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		s.fail(w, err)
		return
	}
	now := s.now()
	err = s.sys.EnableTOTP(ctx, a.User.ID, secret, step, hashes, a.sessionHash, now)
	if errors.Is(err, store.ErrNotFound) {
		fail(http.StatusConflict, msgSetupExpired)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Warn("two-factor enabled", "event", "mfa_enabled", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	if s.mail != nil {
		subject, body := mfaOnMail(s.cfg.BaseURL, now, a.User.Location())
		s.mail.Notice(a.User.Email, subject, body)
	}
	a.User.TOTPEnabledAt, a.MFAAt = &now, &now
	s.showCodes(w, r, a, codes, "Two-factor authentication is on. Your other devices were signed out.")
}

func (s *Server) twoFactorDisable(w http.ResponseWriter, r *http.Request, a *authCtx) {
	fail, ok := s.settingsFail(w, r, a)
	if !ok {
		return
	}
	if !a.twoFactor() {
		redirect(w, r, "/settings/security", msgMFANotOn)
		return
	}
	// Always a code typed now, whatever the session's age.
	if !s.checkFresh(r, a, fail, true) {
		return
	}
	err := s.sys.DisableTOTP(context.WithoutCancel(r.Context()), a.User.ID, a.sessionHash)
	if errors.Is(err, store.ErrNotFound) {
		redirect(w, r, "/settings/security", msgMFANotOn)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Warn("two-factor disabled", "event", "mfa_disabled", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	if s.mail != nil {
		subject, body := mfaOffMail(s.cfg.BaseURL, s.now(), a.User.Location())
		s.mail.Notice(a.User.Email, subject, body)
	}
	redirect(w, r, "/settings/security", msgMFADisabled)
}

func (s *Server) twoFactorRegenerate(w http.ResponseWriter, r *http.Request, a *authCtx) {
	fail, ok := s.settingsFail(w, r, a)
	if !ok {
		return
	}
	if !a.twoFactor() {
		redirect(w, r, "/settings/security", msgMFANotOn)
		return
	}
	if !s.checkFresh(r, a, fail, true) {
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		s.fail(w, err)
		return
	}
	err = s.sys.ReplaceRecoveryCodes(context.WithoutCancel(r.Context()), a.User.ID, hashes, a.sessionHash)
	if errors.Is(err, store.ErrNotFound) {
		redirect(w, r, "/settings/security", msgMFANotOn)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.logger.Warn("recovery codes replaced", "event", "mfa_recovery_replaced", "user_id", a.User.ID, "ip", clientIP(r, s.cfg.ProxyHops()))
	if s.mail != nil {
		subject, body := recoveryNewMail(s.cfg.BaseURL, s.now(), a.User.Location())
		s.mail.Notice(a.User.Email, subject, body)
	}
	s.showCodes(w, r, a, codes, "New recovery codes. The old ones no longer work. Your other devices were signed out.")
}

// twoFactorCancel drops a pending setup. No session is touched.
func (s *Server) twoFactorCancel(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if !a.twoFactor() {
		if err := s.sys.ClearPendingTOTP(context.WithoutCancel(r.Context()), a.User.ID); err != nil {
			s.fail(w, err)
			return
		}
	}
	http.Redirect(w, r, "/settings/security#two-factor", http.StatusSeeOther)
}
