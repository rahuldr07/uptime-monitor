package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rahuldr07/uptime-monitor/internal/store"
)

const (
	sessionCookie     = "uptime_session"
	hostSessionCookie = "__Host-uptime_session"
)

// authCtx is the identity a signed-in request runs as.
type authCtx struct {
	User       store.User
	WS         *store.WS
	Workspace  store.Workspace
	IsOperator bool
	// sessionHash identifies the current session, so a password change can
	// keep it while revoking the rest.
	sessionHash string
	// AuthAt is when the session last authenticated for real; nil for old sessions.
	AuthAt      *time.Time
	HasPassword bool
}

type authKey struct{}

// authResult is filled at most once per request, so one request costs one
// session lookup, including a negative one.
type authResult struct {
	once sync.Once
	a    *authCtx
	ok   bool
}

// cookieName is the session cookie name for this request. The __Host- prefix
// pins the cookie to this exact host over HTTPS.
func (s *Server) cookieName(r *http.Request) string {
	if s.isHTTPS(r) || s.cfg.HTTPSBase() {
		return hostSessionCookie
	}
	return sessionCookie
}

// sessionToken returns the raw session token under the one cookie name this
// request may use. Over HTTPS only the __Host- cookie counts, so a plain
// cookie planted from a sibling subdomain is ignored.
func (s *Server) sessionToken(r *http.Request) string {
	if c, err := r.Cookie(s.cookieName(r)); err == nil {
		return c.Value
	}
	return ""
}

// currentAuth resolves the request's session, caching the result in the
// request context (installed by withAuthCache).
func (s *Server) currentAuth(r *http.Request) (*authCtx, bool) {
	if res, ok := r.Context().Value(authKey{}).(*authResult); ok {
		res.once.Do(func() { res.a, res.ok = s.lookupAuth(r) })
		return res.a, res.ok
	}
	return s.lookupAuth(r)
}

func (s *Server) lookupAuth(r *http.Request) (*authCtx, bool) {
	raw := s.sessionToken(r)
	if raw == "" {
		return nil, false
	}
	au, err := s.sys.SessionAuth(r.Context(), raw, s.now())
	if err != nil {
		return nil, false
	}
	return &authCtx{User: au.User, WS: au.WS, Workspace: au.Workspace,
		IsOperator: au.User.IsOperator, sessionHash: au.SessionHash, AuthAt: au.AuthAt, HasPassword: au.HasPassword}, true
}

// withAuthCache installs the per-request cache that currentAuth fills. It
// also gates /admin: whatever the method, origin or sub-path, a request that
// is not from a signed-in operator gets 404 before the mux can answer 403 or
// 405 and reveal the panel.
func (s *Server) withAuthCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), authKey{}, &authResult{}))
		if p := r.URL.Path; p == "/admin" || strings.HasPrefix(p, "/admin/") {
			if a, ok := s.currentAuth(r); !ok || !a.User.IsOperator {
				http.NotFound(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, raw string) {
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(r), Value: raw, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.cookieName(r) == hostSessionCookie,
		MaxAge: int(store.SessionTTL.Seconds()),
	})
}

// clearSessionCookie expires the session cookie under both names.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{hostSessionCookie, sessionCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, Secure: name == hostSessionCookie, MaxAge: -1})
	}
}

// requireUser runs h for signed-in requests. Programmatic callers get a 401
// they can act on instead of a redirect that fetch() would follow into the
// login page.
func (s *Server) requireUser(h func(http.ResponseWriter, *http.Request, *authCtx)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a, ok := s.currentAuth(r); ok {
			h(w, r, a)
			return
		}
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

// requireOperator runs h for signed-in operators and answers 404 to everyone
// else, anonymous included: the admin panel does not reveal that it exists.
// The /admin gate in withAuthCache already filters; this is the second line.
func (s *Server) requireOperator(h func(http.ResponseWriter, *http.Request, *authCtx)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a, ok := s.currentAuth(r); ok && a.User.IsOperator {
			h(w, r, a)
			return
		}
		http.NotFound(w, r)
	}
}

// adminIndex is a placeholder landing route for the panel.
func (s *Server) adminIndex(w http.ResponseWriter, r *http.Request, a *authCtx) {
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// setupGate confines the app to the setup flow until the first account
// exists.
func (s *Server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupMode.Load() {
			next.ServeHTTP(w, r)
			return
		}
		p, get := r.URL.Path, r.Method == http.MethodGet || r.Method == http.MethodHead
		switch {
		case get && (p == "/healthz" || p == "/status" || strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/s/")):
		case p == "/setup" && (get || r.Method == http.MethodPost):
		case strings.HasPrefix(p, "/api/") || p == "/events":
			http.Error(w, "setup required", http.StatusServiceUnavailable)
			return
		default:
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isHTTPS reports whether the client connected over TLS. X-Forwarded-Proto
// is only believed when trust_proxy is set, since anyone can send it.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return s.cfg.TrustProxy && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
