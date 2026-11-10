package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

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

// passwordEqual compares in constant time. Hashing first gives both sides
// the same length, since ConstantTimeCompare returns early on a length
// mismatch and would otherwise leak the password's length.
func passwordEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

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
