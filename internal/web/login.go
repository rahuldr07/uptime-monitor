package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
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

// loginMaxFailures is how many failed logins a client may make per minute.
// Successful logins are not counted, so the admin is never locked out by
// their own use; attackers sharing the admin's IP (or proxy, when
// trust_proxy is off) still get throttled.
const loginMaxFailures = 5

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
	if ok, _ := s.limiter.Allow(key); !ok {
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
	s.limiter.Hit(key)
	d.Error = "Wrong password."
	w.WriteHeader(http.StatusUnauthorized)
	s.render(w, r, "login", d)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w, r)
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}
