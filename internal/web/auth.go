package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
func sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: int(sessionTTL.Seconds()),
	})
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r), MaxAge: -1})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
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

// clientKey returns the client IP. Behind a reverse proxy, set trust_proxy
// so the first X-Forwarded-For address is used instead of the proxy's.
func clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := strings.TrimSpace(strings.Split(xff, ",")[0])
			if ip := net.ParseIP(first); ip != nil {
				return ip.String()
			}
		}
		if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func passwordEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
