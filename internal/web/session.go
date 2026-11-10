package web

import (
	"crypto/rand"
	"encoding/hex"
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
