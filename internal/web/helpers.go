package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ---- helpers ----

// render writes a full page, or only the content block for live refreshes.
// The page is rendered into a buffer first so a template error becomes a
// clean 500 instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	b, err := s.renderBytes(r, page, data)
	if err != nil {
		s.fail(w, fmt.Errorf("render %s: %w", page, err))
		return
	}
	writeHTML(w, b)
}

func (s *Server) renderBytes(r *http.Request, page string, data any) ([]byte, error) {
	name := "layout"
	if r.Header.Get("X-Partial") == "1" {
		name = "content"
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func redirect(w http.ResponseWriter, r *http.Request, to, msg string) {
	http.Redirect(w, r, withFlash(to, msg), http.StatusSeeOther)
}

// flashKey signs flash messages carried in ?msg=, so a crafted link cannot
// make the admin UI display attacker-chosen text as if the app said it. It
// is random per process; a restart only drops messages in flight.
var flashKey = func() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return b
}()

func flashSig(msg string) string {
	m := hmac.New(sha256.New, flashKey)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

// withFlash appends a signed flash message to a local URL.
func withFlash(to, msg string) string {
	if msg == "" {
		return to
	}
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	return to + sep + "msg=" + url.QueryEscape(msg) + "&sig=" + flashSig(msg)
}

// flashFrom returns the request's flash message if its signature is valid.
func flashFrom(r *http.Request) string {
	q := r.URL.Query()
	msg := q.Get("msg")
	if msg == "" || !hmac.Equal([]byte(q.Get("sig")), []byte(flashSig(msg))) {
		return ""
	}
	return msg
}

// backTo returns the Referer path and query (minus any old flash) when it
// is a local page, so filters and tabs survive a round trip; else fallback.
func backTo(r *http.Request, fallback string) string {
	ref, err := url.Parse(r.Referer())
	if err != nil || ref.Host != r.Host || !strings.HasPrefix(ref.Path, "/") {
		return fallback
	}
	q := ref.Query()
	q.Del("msg")
	q.Del("sig")
	if len(q) > 0 {
		return ref.Path + "?" + q.Encode()
	}
	return ref.Path
}

func writeJSON(w http.ResponseWriter, v any) { writeJSONStatus(w, http.StatusOK, v) }

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// shortDur renders durations the way people type them: 30s, 2m, 1h30m.
func shortDur(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if sec > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", sec)
	}
	return b.String()
}

func humanAgo(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

func formatTS(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "—"
		}
		t = *x
	default:
		return "—"
	}
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("Jan 2 15:04:05")
}
