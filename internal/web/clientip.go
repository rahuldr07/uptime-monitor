package web

import (
	"net"
	"net/http"
	"strings"
)

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
