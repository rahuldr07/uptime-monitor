package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/notify"
	"github.com/rahuldr07/uptime-monitor/internal/store"
)

// ---- helpers ----

// render writes a full page, or only the content block for live refreshes.
// The page is rendered into a buffer first so a template error becomes a
// clean 500 instead of a half-written page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data any) {
	s.renderStatus(w, r, http.StatusOK, page, data)
}

// renderStatus is render with an explicit status code. The body is built
// first, so a template error is a 500 rather than a 4xx with a broken page.
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, code int, page string, data any) {
	b, err := s.renderBytes(r, page, data)
	if err != nil {
		s.fail(w, fmt.Errorf("render %s: %w", page, err))
		return
	}
	writeHTML(w, code, b)
}

// loadByID parses the {id} path value and loads it with get. A bad id or
// store.ErrNotFound is a 404, any other error a 500; both report false.
func loadByID[T any](s *Server, w http.ResponseWriter, r *http.Request, get func(context.Context, int64) (T, error)) (T, bool) {
	var zero T
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return zero, false
	}
	v, err := get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return zero, false
	}
	if err != nil {
		s.fail(w, err)
		return zero, false
	}
	return v, true
}

func (s *Server) renderBytes(r *http.Request, page string, data any) ([]byte, error) {
	return s.renderPage(page, r.Header.Get("X-Partial") == "1", data)
}

// renderPage renders the full page, or only its content block when partial.
func (s *Server) renderPage(page string, partial bool, data any) ([]byte, error) {
	name := "layout"
	if partial {
		name = "content"
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, name, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeHTML(w http.ResponseWriter, code int, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(b)
}

type notFoundData struct {
	base
	Path       string // echoed to signed-in users only
	Status     bool   // under /s/: a missing status page
	StatusLink bool   // the general status page exists, so link to it
}

// notFound answers 404. Browsers navigating by GET get a page that says so
// (inside the layout when signed in); partial refreshes, the JSON API, JSON
// callers and non-GET requests get the plain 404 they can act on.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || r.Header.Get("X-Partial") == "1" ||
		strings.HasPrefix(r.URL.Path, "/api/") || wantsJSON(r) {
		http.NotFound(w, r) // bare, so it matches the mux's own 404 for unknown paths (the /admin gate relies on that)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	// /s/ may be framed by other sites, but this page must not be: it can render the signed-in layout.
	h.Set("Content-Security-Policy", cspBase+"frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	d := notFoundData{Status: strings.HasPrefix(r.URL.Path, "/s/")}
	a, signedIn := s.currentAuth(r)
	if signedIn && !d.Status {
		p := []rune(r.URL.Path)
		if len(p) > 80 {
			p = append(p[:80], '…')
		}
		d.Path = string(p)
		d.base = s.base(r, a, "", "Page not found")
		d.NoRefresh = true
		s.renderStatus(w, r, http.StatusNotFound, "notfound", d)
		return
	}
	d.base = s.page(r, "Page not found")
	if d.Status {
		d.Title = "Status page not found"
	} else if _, err := s.sys.Workspace(store.OperatorWorkspaceID).Info(r.Context()); err == nil {
		d.StatusLink = true
	}
	s.renderStatus(w, r, http.StatusNotFound, "notfound_public", d)
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

// formatTS renders an instant in loc (nil means UTC) as "Jan 2 15:04:05".
func formatTS(loc *time.Location, v any) string {
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
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("Jan 2 15:04:05")
}

// formatTSZone renders an instant for the public status page, whose visitors
// did not choose the zone: a <time> element reading "Jan 2 15:04 IST", without
// seconds.
func formatTSZone(loc *time.Location, v any) template.HTML {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x != nil {
			t = *x
		}
	}
	if t.IsZero() {
		return "—"
	}
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	return template.HTML(`<time datetime="` + t.UTC().Format(time.RFC3339) + `">` +
		template.HTMLEscapeString(t.Format("Jan 2 15:04 MST")) + `</time>`)
}

// zoneAbbr is the abbreviation of loc at t: "IST", "UTC", or "+03" where the
// zone has no letters.
func zoneAbbr(loc *time.Location, t time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Format("MST")
}

// zoneNote names a zone for the footer and Account page: "IST (Asia/Kolkata)",
// or just "UTC". An alias such as Asia/Calcutta is shown as its listed name.
func zoneNote(loc *time.Location, at time.Time) string {
	if loc == nil || loc.String() == "UTC" {
		return "UTC"
	}
	return zoneAbbr(loc, at) + " (" + store.CanonicalZone(loc.String()) + ")"
}

// zoneOffset renders loc's offset from UTC at t as "UTC+05:30" (a real minus
// sign when negative); UTC if loc is nil.
func zoneOffset(loc *time.Location, t time.Time) string {
	if loc == nil {
		loc = time.UTC
	}
	return strings.Replace(notify.UTCOffset(t.In(loc)), "-", "−", 1)
}
