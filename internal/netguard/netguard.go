// Package netguard stops the server from connecting to internal addresses
// (SSRF protection). It is the only place allowed to build HTTP clients,
// transports and dialers: the check runs at connect time, on the address
// actually being dialled after DNS resolution, so DNS rebinding, redirects
// and hostnames that resolve to internal addresses are all covered. Code
// elsewhere must obtain its clients from here rather than constructing its own.
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// ErrBlocked is returned when a destination is an internal address. It never
// contains the address itself.
var ErrBlocked = errors.New("blocked: destination is an internal address")

// blockedPrefixes are pinned from the IANA special-purpose registries.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24",
		"192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24",
		"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"::/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23",
		"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7",
		"fe80::/10", "fec0::/10",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Blocked reports whether a is not an allowed destination.
func Blocked(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || !a.IsGlobalUnicast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Dialer returns a dialer that refuses internal addresses unless allowPrivate.
func Dialer(allowPrivate bool, timeout time.Duration) *net.Dialer {
	if allowPrivate {
		return &net.Dialer{Timeout: timeout}
	}
	return newDialer(timeout, Blocked)
}

func newDialer(timeout time.Duration, blocked func(netip.Addr) bool) *net.Dialer {
	return &net.Dialer{
		Timeout: timeout,
		ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || blocked(ap.Addr()) {
				return ErrBlocked
			}
			return nil
		},
	}
}

// Transport returns a clone of http.DefaultTransport using Dialer. Guarded
// transports ignore proxy environment variables, since a proxy would be the
// connection target and bypass the check. Callers may adjust fields on the
// result but must not replace the dial hooks.
func Transport(allowPrivate bool, dialTimeout time.Duration) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = Dialer(allowPrivate, dialTimeout).DialContext
	t.DialTLSContext, t.DialTLS, t.Dial = nil, nil, nil
	if allowPrivate {
		t.Proxy = http.ProxyFromEnvironment
	} else {
		t.Proxy = nil
	}
	return t
}

// Client returns an http.Client built on Transport.
func Client(allowPrivate bool, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(allowPrivate, 10*time.Second)}
}

// CheckHost gives an early error at save time for obviously internal hosts.
// The connect-time guard remains the real enforcement.
func CheckHost(host string) error {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return ErrBlocked
	}
	if a, err := netip.ParseAddr(h); err == nil && Blocked(a) {
		return ErrBlocked
	}
	return nil
}
