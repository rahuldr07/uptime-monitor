package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "0.0.0.0", "::", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "fd00::1", "fe80::1", "fec0::1", "240.0.0.1",
		"255.255.255.255", "224.0.0.1", "192.0.0.1", "192.88.99.1", "198.18.0.1",
		"100::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::1", "2001::1",
		"2001:db8::1", "3fff::1", "fd00::1%eth0", "fe80::1%eth0", "::7f00:1",
	}
	for _, s := range blocked {
		if !Blocked(netip.MustParseAddr(s)) {
			t.Errorf("Blocked(%s) = false, want true", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111", "2a00:1450:4001::1"}
	for _, s := range allowed {
		if Blocked(netip.MustParseAddr(s)) {
			t.Errorf("Blocked(%s) = true, want false", s)
		}
	}
	if !Blocked(netip.Addr{}) {
		t.Error("Blocked(zero Addr) = false, want true")
	}
}

func okServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return srv, u.Port()
}

func TestGuardedDialRefusesLoopback(t *testing.T) {
	_, port := okServer(t)
	c := Client(false, 5*time.Second)
	for _, host := range []string{"127.0.0.1", "localhost"} {
		_, err := c.Get("http://" + host + ":" + port + "/")
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("guarded Get %s: err = %v, want ErrBlocked", host, err)
		}
	}
	resp, err := Client(true, 5*time.Second).Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatalf("unguarded Get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestGuardedRedirectRefused(t *testing.T) {
	target, _ := okServer(t)
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skipf("cannot listen on 127.0.0.2: %v", err)
	}
	var hits atomic.Int32
	redirect := http.RedirectHandler(target.URL, http.StatusFound)
	front := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		redirect.ServeHTTP(w, r)
	})}
	go front.Serve(ln)
	t.Cleanup(func() { front.Close() })

	tr := Transport(false, 5*time.Second)
	tr.DialContext = newDialer(5*time.Second, func(a netip.Addr) bool {
		return a != netip.MustParseAddr("127.0.0.2")
	}).DialContext
	c := &http.Client{Transport: tr}
	_, err = c.Get("http://" + ln.Addr().String() + "/")
	if !errors.Is(err, ErrBlocked) {
		t.Errorf("redirect to loopback: err = %v, want ErrBlocked", err)
	}
	if hits.Load() != 1 {
		t.Errorf("front hit %d times, want 1 (redirect never happened)", hits.Load())
	}
}

func TestGuardedTransportIgnoresProxy(t *testing.T) {
	if Transport(false, time.Second).Proxy != nil {
		t.Error("guarded transport has a Proxy")
	}
	if Transport(true, time.Second).Proxy == nil {
		t.Error("unguarded transport lost its Proxy")
	}
}

func TestCheckHost(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST", "api.localhost", "localhost.", "api.localhost.", "127.0.0.1", "::1", "169.254.169.254", "10.0.0.5"} {
		if err := CheckHost(h); !errors.Is(err, ErrBlocked) {
			t.Errorf("CheckHost(%q) = %v, want ErrBlocked", h, err)
		}
	}
	for _, h := range []string{"example.com", "8.8.8.8", "2606:4700:4700::1111", ""} {
		if err := CheckHost(h); err != nil {
			t.Errorf("CheckHost(%q) = %v, want nil", h, err)
		}
	}
}

func TestControlRejectsUnparsableAddress(t *testing.T) {
	d := Dialer(false, time.Second)
	if err := d.ControlContext(context.Background(), "tcp", "not-an-addr", nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("err = %v, want ErrBlocked", err)
	}
}

func TestPrefixListEntriesBlocked(t *testing.T) {
	for _, p := range blockedPrefixes {
		if !Blocked(p.Addr()) {
			t.Errorf("first address of %s is not blocked", p)
		}
	}
}
