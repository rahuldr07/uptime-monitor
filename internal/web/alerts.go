package web

import (
	"context"
	"encoding/json"
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

// notifyConfig returns the stored notify settings, falling back to the config file.
func (s *Server) notifyConfig(ctx context.Context) config.Notify {
	raw, err := s.store.GetSetting(ctx, "notify")
	if err == nil {
		var n config.Notify
		if json.Unmarshal([]byte(raw), &n) == nil {
			return n
		}
	}
	return s.cfg.Notify
}

func (s *Server) alertsPage(w http.ResponseWriter, r *http.Request) {
	s.renderAlerts(w, r, http.StatusOK, s.notifyConfig(r.Context()), "")
}

func (s *Server) renderAlerts(w http.ResponseWriter, r *http.Request, code int, n config.Notify, errMsg string) {
	d := alertsData{base: s.base(r, "alerts", "Alert channels"), Has: map[string]bool{}, Threshold: s.cfg.FailureThreshold,
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
	stored := s.notifyConfig(r.Context())
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
	for _, nn := range s.mon.Notifiers(r.Context(), s.ws(r).ID()) {
		d.Has[nn.Name] = true
	}
	d.Last, _ = s.store.LastAlerts(r.Context())
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
		if prev.Telegram != nil {
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
		var old string
		if prev.Email != nil {
			old = prev.Email.Password
		}
		n.Email = &config.Email{SMTPHost: v("smtp_host"), SMTPPort: port, Username: v("smtp_user"),
			Password: keep("smtp_pass", old), From: v("smtp_from"), To: to, Disabled: v("email_enabled") != "1"}
	}
	return n
}

// saveNotify validates and persists settings, swapping the live notifiers.
func (s *Server) saveNotify(w http.ResponseWriter, r *http.Request) (config.Notify, bool) {
	n := parseNotifyForm(r, s.notifyConfig(r.Context()))
	if err := config.ValidateNotify(&n); err != nil {
		s.renderAlerts(w, r, http.StatusUnprocessableEntity, n, err.Error())
		return n, false
	}
	raw, _ := json.Marshal(n)
	if err := s.store.SetSetting(r.Context(), "notify", string(raw)); err != nil {
		s.fail(w, err)
		return n, false
	}
	ws := s.ws(r)
	s.mon.SetNotifiers(ws.ID(), notify.FromConfig(n, s.logger, s.cfg.AllowPrivate(ws.ID() == pinnedWorkspace)))
	return n, true
}

func (s *Server) alertsSave(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.saveNotify(w, r); !ok {
		return
	}
	redirect(w, r, "/alerts", "Alert channels saved.")
}

// alertsTest saves the form first, then sends a test through one channel.
func (s *Server) alertsTest(w http.ResponseWriter, r *http.Request) {
	n, ok := s.saveNotify(w, r)
	if !ok {
		return
	}
	ch := r.FormValue("channel")
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.mon.SendTest(ctx, s.ws(r).ID(), ch); err != nil {
		s.logger.Warn("test alert failed", "channel", ch, "err", err)
		s.renderAlerts(w, r, http.StatusBadGateway, n, "Settings saved, but the test to "+ch+" failed: "+err.Error())
		return
	}
	redirect(w, r, "/alerts", "Settings saved and test alert sent to "+ch+".")
}
