package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

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
		if strings.HasPrefix(r.URL.Path, "/s/") && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
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
