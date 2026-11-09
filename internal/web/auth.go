package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "uptime_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// sessions is an in-memory session store. Sessions do not survive a restart,
// which is acceptable for a single-admin tool and means logout truly revokes.
type sessions struct {
	mu   sync.Mutex
	live map[string]time.Time // id -> expiry
	now  func() time.Time
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{live: map[string]time.Time{}, now: now}
}

func (s *sessions) create() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	id := hex.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for k, exp := range s.live {
		if !exp.After(now) {
			delete(s.live, k)
		}
	}
	s.live[id] = now.Add(sessionTTL)
	return id
}

func (s *sessions) valid(id string) bool {
	if len(id) != 64 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.live[id]
	return ok && exp.After(s.now())
}

func (s *sessions) revoke(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.live, id)
}

// isAdmin reports whether the request carries a valid session, or auth is off.
func (s *Server) isAdmin(r *http.Request) bool {
	if !s.authEnabled() {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(c.Value)
}

func (s *Server) authEnabled() bool { return s.cfg.AdminPassword != "" }

// requireAdmin redirects anonymous users to the login page.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.isAdmin(r) {
			next(w, r)
			return
		}
		// Programmatic callers get a 401 they can act on instead of a redirect
		// that fetch() would follow into the login page.
		if strings.HasPrefix(r.URL.Path, "/api/") || r.Header.Get("X-Partial") == "1" || wantsJSON(r) ||
			strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next := r.URL.RequestURI()
		if r.Method != http.MethodGet {
			next = "/"
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
	}
}

// sameOrigin rejects state-changing requests that a browser reports as
// coming from another site. It protects against CSRF even when auth is off.
// Requests without Origin/Referer/Sec-Fetch-Site (curl, scripts) are allowed.
// It also caps the request body, since every state-changing route goes
// through here.
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := parseForm(r); err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		for _, h := range []string{"Origin", "Referer"} {
			v := r.Header.Get(h)
			if v == "" {
				continue
			}
			// "Origin: null" (sandboxed iframes, data: pages) is cross-site, so
			// let it fall through to the host comparison, which fails.
			u, err := url.Parse(v)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "cross-site request rejected", http.StatusForbidden)
				return
			}
			break // Origin is authoritative when present
		}
		next(w, r)
	}
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.sessions.create(), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r), MaxAge: int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.isHTTPS(r), MaxAge: -1})
}

// isHTTPS reports whether the client connected over TLS. X-Forwarded-Proto
// is only believed when trust_proxy is set, since anyone can send it.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

const maxBodyBytes = 1 << 20

// parseForm parses url-encoded and multipart bodies (the UI's fetch calls
// send FormData) so size errors surface here rather than as empty fields.
func parseForm(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(maxBodyBytes)
	}
	return r.ParseForm()
}

// cspBase allows only same-origin scripts (no inline handlers), inline
// styles used by the templates, and data: URIs for the favicon.
const cspBase = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; object-src 'none'; "

// securityHeaders sets browser hardening headers on every response.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		// The public status page may be embedded in other sites. That is safe:
		// the SameSite=Lax session cookie is not sent to a cross-site frame, so
		// a framed page is always the anonymous, read-only view. Everything
		// else refuses framing (clickjacking).
		if r.URL.Path == "/status" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			h.Set("Content-Security-Policy", cspBase+"frame-ancestors *")
		} else {
			h.Set("Content-Security-Policy", cspBase+"frame-ancestors 'none'")
			h.Set("X-Frame-Options", "DENY")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// HSTS is deliberately left to the TLS-terminating proxy: it pins
		// browsers to HTTPS for its whole max-age, which the app cannot undo.
		next.ServeHTTP(w, r)
	})
}

// safeNext only allows plain local paths as post-login redirect targets.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/login") || strings.ContainsAny(next, "\\\r\n\t ") {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") {
		return "/"
	}
	return next
}

// loginLimiter allows a handful of failed attempts per client per minute.
// Successful logins are not counted, so the admin is never locked out by
// their own use; attackers sharing the admin's IP (or proxy, when
// trust_proxy is off) still get throttled.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	now      func() time.Time
}

func newLoginLimiter(now func() time.Time) *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}, now: now}
}

const loginMaxFailures = 5

// blocked reports whether the key has too many recent failures.
func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= loginMaxFailures
}

// fail records a failed attempt.
func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key] = append(l.prune(key), l.now())
	if len(l.failures) > 10000 { // evict stale keys, never wipe live ones
		for k := range l.failures {
			if len(l.prune(k)) == 0 {
				delete(l.failures, k)
			}
		}
	}
}

// prune drops failures older than a minute; caller holds the lock.
func (l *loginLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-time.Minute)
	kept := l.failures[key][:0]
	for _, t := range l.failures[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, key)
		return nil
	}
	l.failures[key] = kept
	return kept
}

// clientKey returns the client IP used for login rate limiting. With
// proxyHops > 0 (trust_proxy), the address is taken from X-Forwarded-For,
// counting proxyHops entries from the right. Each proxy appends the address
// it received the request from, so only the rightmost entries are
// trustworthy; anything to their left was sent by the client and taking it
// would let an attacker pick a fresh "IP" for every guess.
func clientKey(r *http.Request, proxyHops int) string {
	if proxyHops > 0 {
		var parts []string
		for _, h := range r.Header.Values("X-Forwarded-For") {
			parts = append(parts, strings.Split(h, ",")...)
		}
		if len(parts) > 0 {
			i := len(parts) - proxyHops
			if i < 0 {
				i = 0 // fewer entries than hops: the leftmost was added by our first proxy
			}
			if ip := parseAddr(parts[i]); ip != nil {
				return ip.String()
			}
		} else if ip := parseAddr(r.Header.Get("X-Real-IP")); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// parseAddr parses a forwarded address that may carry a port, as some load
// balancers send ("1.2.3.4:5678", "[2001:db8::1]:443"). Without this every
// client would fall back to the proxy's address and share one limit bucket.
func parseAddr(s string) net.IP {
	s = strings.TrimSpace(s)
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	return net.ParseIP(strings.Trim(s, "[]"))
}

// passwordEqual compares in constant time. Hashing first gives both sides
// the same length, since ConstantTimeCompare returns early on a length
// mismatch and would otherwise leak the password's length.
func passwordEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// ---- auth pages ----

type loginData struct {
	base
	Next string
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() || s.isAdmin(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", loginData{base: s.base(r, "login", "Log in · Uptime"), Next: safeNext(r.URL.Query().Get("next"))})
}

func (s *Server) loginPost(w http.ResponseWriter, r *http.Request) {
	if !s.authEnabled() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	d := loginData{base: s.base(r, "login", "Log in · Uptime"), Next: safeNext(r.FormValue("next"))}
	key := clientKey(r, s.cfg.ProxyHops())
	if s.limiter.blocked(key) {
		d.Error = "Too many failed attempts. Wait a minute and try again."
		w.WriteHeader(http.StatusTooManyRequests)
		s.render(w, r, "login", d)
		return
	}
	if passwordEqual(r.FormValue("password"), s.cfg.AdminPassword) {
		s.setSession(w, r)
		http.Redirect(w, r, d.Next, http.StatusSeeOther)
		return
	}
	s.limiter.fail(key)
	d.Error = "Wrong password."
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, r, "login", d)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w, r)
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}
