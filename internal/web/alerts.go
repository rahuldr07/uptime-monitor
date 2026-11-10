package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"uptimemon/internal/config"
	"uptimemon/internal/notify"
	"uptimemon/internal/store"
)

// ---- alerts ----

type alertsData struct {
	base
	N         config.Notify // secrets blanked; see Saved
	To        string
	Has       map[string]bool
	Last      map[string]store.Alert
	Threshold int
	// Saved marks secrets (tg_token, slack_url, smtp_pass) already stored.
	// They are never sent back to the browser; a blank field keeps them.
	Saved map[string]bool
	// Stored marks channels that have saved settings and can be removed.
	Stored map[string]bool
}

// notifyConfig returns the workspace's stored alert channels; nothing stored means none.
func (s *Server) notifyConfig(ctx context.Context, ws *store.WS) config.Notify {
	n, err := ws.Notify(ctx)
	if err != nil {
		s.logger.Warn("read alert channels", "workspace", ws.ID(), "err", err)
	}
	return n
}

func (s *Server) alertsPage(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	n, err := ws.Notify(r.Context())
	errMsg := ""
	if err != nil {
		s.logger.Warn("read alert channels", "workspace", ws.ID(), "err", err)
		errMsg = "Stored alert settings could not be read; saving this form will replace them."
	}
	s.renderAlerts(w, r, a, http.StatusOK, n, errMsg)
}

func (s *Server) renderAlerts(w http.ResponseWriter, r *http.Request, a *authCtx, code int, n config.Notify, errMsg string) {
	ws := a.WS
	d := alertsData{base: s.base(r, a, "alerts", "Alert channels"), Has: map[string]bool{}, Threshold: s.cfg.FailureThreshold,
		Saved: map[string]bool{}, Stored: map[string]bool{}}
	d.Error = errMsg
	// Copy each channel so blanking secrets cannot touch shared config.
	tg, sl, em := config.Telegram{}, config.Slack{}, config.Email{}
	if n.Telegram != nil {
		tg = *n.Telegram
	}
	if n.Slack != nil {
		sl = *n.Slack
	}
	if n.Email != nil {
		em = *n.Email
	}
	stored := s.notifyConfig(r.Context(), ws)
	if t := stored.Telegram; t != nil {
		d.Stored["telegram"], d.Saved["tg_token"] = true, t.BotToken != ""
	}
	if sk := stored.Slack; sk != nil {
		d.Stored["slack"], d.Saved["slack_url"] = true, sk.WebhookURL != ""
	}
	if e := stored.Email; e != nil {
		d.Stored["email"], d.Saved["smtp_pass"] = true, e.Password != ""
	}
	tg.BotToken, sl.WebhookURL, em.Password = "", "", ""
	d.N = config.Notify{Telegram: &tg, Slack: &sl, Email: &em}
	d.To = strings.Join(em.To, ", ")
	for _, nn := range s.mon.Notifiers(r.Context(), ws.ID()) {
		d.Has[nn.Name] = true
	}
	d.Last, _ = ws.LastAlerts(r.Context())
	s.renderStatus(w, r, code, "alerts", d)
}

// parseNotifyForm builds notify settings from the alerts form. Secret
// fields left blank keep the value in prev; a channel's "remove" box
// drops it entirely.
func parseNotifyForm(r *http.Request, prev config.Notify) config.Notify {
	v := func(k string) string { return strings.TrimSpace(r.FormValue(k)) }
	keep := func(field, old string) string {
		if s := v(field); s != "" {
			return s
		}
		return old
	}
	var n config.Notify
	// Clearing the visible chat ID (with no new token) removes Telegram, the
	// same way clearing the email fields removes email.
	if v("tg_clear") != "1" && (v("tg_chat") != "" || v("tg_token") != "") {
		var old string
		if prev.Telegram != nil && prev.Telegram.ChatID == v("tg_chat") {
			old = prev.Telegram.BotToken
		}
		n.Telegram = &config.Telegram{BotToken: keep("tg_token", old), ChatID: v("tg_chat"), Disabled: v("tg_enabled") != "1"}
	}
	if v("slack_clear") != "1" {
		var old string
		if prev.Slack != nil {
			old = prev.Slack.WebhookURL
		}
		if hook := keep("slack_url", old); hook != "" {
			n.Slack = &config.Slack{WebhookURL: hook, Disabled: v("slack_enabled") != "1"}
		}
	}
	if v("email_clear") != "1" && (v("smtp_host") != "" || v("smtp_from") != "" || v("smtp_to") != "") {
		port, _ := strconv.Atoi(v("smtp_port"))
		var to []string
		for _, a := range strings.Split(v("smtp_to"), ",") {
			if a = strings.TrimSpace(a); a != "" {
				to = append(to, a)
			}
		}
		// A saved password follows its destination: a new host, port or
		// username must not receive it.
		var old string
		if pe := prev.Email; pe != nil && pe.SMTPHost == v("smtp_host") && smtpPort(pe.SMTPPort) == smtpPort(port) && pe.Username == v("smtp_user") {
			old = pe.Password
		}
		n.Email = &config.Email{SMTPHost: v("smtp_host"), SMTPPort: port, Username: v("smtp_user"),
			Password: keep("smtp_pass", old), From: v("smtp_from"), To: to, Disabled: v("email_enabled") != "1"}
	}
	return n
}

// smtpPort maps the unset port to the default so it compares with a saved one.
func smtpPort(p int) int {
	if p == 0 {
		return 587
	}
	return p
}

// clearedSecret names a saved secret the form dropped because its
// destination changed and no new value was given; "" if none.
func clearedSecret(prev, n config.Notify) string {
	if prev.Telegram != nil && prev.Telegram.BotToken != "" && n.Telegram != nil && n.Telegram.BotToken == "" {
		return "telegram: the chat ID changed, so re-enter the bot token"
	}
	if prev.Email != nil && prev.Email.Password != "" && n.Email != nil && n.Email.Password == "" {
		return "email: the SMTP host, port or username changed, so re-enter the password"
	}
	return ""
}

// saveNotify validates and persists settings, swapping the live notifiers.
func (s *Server) saveNotify(w http.ResponseWriter, r *http.Request, a *authCtx) (config.Notify, bool) {
	ws := a.WS
	prev := s.notifyConfig(r.Context(), ws)
	n := parseNotifyForm(r, prev)
	validate := config.ValidateNotifyRestricted
	if a.IsOperator {
		validate = config.ValidateNotify
	}
	err := validate(&n)
	if msg := clearedSecret(prev, n); msg != "" {
		err = errors.New(msg)
	}
	if err != nil {
		s.renderAlerts(w, r, a, http.StatusUnprocessableEntity, n, err.Error())
		return n, false
	}
	raw, _ := json.Marshal(n)
	if err := ws.SetSetting(r.Context(), "notify", string(raw)); err != nil {
		s.fail(w, err)
		return n, false
	}
	s.mon.SetNotifiers(ws.ID(), notify.FromConfig(n, s.logger, s.cfg.AllowPrivate(a.IsOperator), !a.IsOperator))
	return n, true
}

func (s *Server) alertsSave(w http.ResponseWriter, r *http.Request, a *authCtx) {
	if _, ok := s.saveNotify(w, r, a); !ok {
		return
	}
	redirect(w, r, "/alerts", "Alert channels saved.")
}

// alertsTest saves the form first, then sends a test through one channel.
func (s *Server) alertsTest(w http.ResponseWriter, r *http.Request, a *authCtx) {
	ws := a.WS
	if !a.IsOperator {
		if ok, retry := s.alertTest.Take(wsKey(a)); !ok {
			retryAfter(w, retry)
			n := parseNotifyForm(r, s.notifyConfig(r.Context(), ws))
			s.renderAlerts(w, r, a, http.StatusTooManyRequests, n, "Too many test messages. Wait a minute and try again. Settings were not saved.")
			return
		}
	}
	n, ok := s.saveNotify(w, r, a)
	if !ok {
		return
	}
	ch := r.FormValue("channel")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.mon.SendTest(ctx, ws.ID(), ch); err != nil {
		s.logger.Warn("test alert failed", "channel", ch, "err", err)
		s.renderAlerts(w, r, a, http.StatusBadGateway, n, "Settings saved, but the test to "+ch+" failed: "+err.Error())
		return
	}
	redirect(w, r, "/alerts", "Settings saved and test alert sent to "+ch+".")
}
