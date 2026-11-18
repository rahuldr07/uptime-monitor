package totp

import (
	"encoding/base32"
	"regexp"
	"strings"
	"testing"
	"time"
)

var rfcSecret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("12345678901234567890"))

func TestRFC6238Vectors(t *testing.T) {
	for _, c := range []struct {
		unix int64
		want string
	}{
		{59, "287082"},
		{1111111109, "081804"},
		{1111111111, "050471"},
		{1234567890, "005924"},
		{2000000000, "279037"},
	} {
		got, err := Code(rfcSecret, Step(time.Unix(c.unix, 0)))
		if err != nil || got != c.want {
			t.Errorf("T=%d: got %q, %v; want %q", c.unix, got, err, c.want)
		}
	}
}

func TestVerifyWindow(t *testing.T) {
	now := time.Unix(1234567890, 0)
	step := Step(now)
	for off := int64(-3); off <= 3; off++ {
		code, err := Code(rfcSecret, step+off)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := Match(rfcSecret, code, now)
		inWindow := off >= -1 && off <= 1
		if ok != inWindow {
			t.Errorf("offset %d: ok = %v, want %v", off, ok, inWindow)
		}
		if ok && got != step+off {
			t.Errorf("offset %d: step = %d, want %d", off, got, step+off)
		}
	}
	if _, ok := Match(rfcSecret, "12345", now); ok {
		t.Error("short code accepted")
	}
	if _, ok := Match("not base32 !!", "123456", now); ok {
		t.Error("bad secret accepted")
	}
}

func TestNewSecret(t *testing.T) {
	a, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSecret()
	if a == b || len(a) != 32 || a != strings.ToUpper(a) || strings.Contains(a, "=") {
		t.Fatalf("secrets %q %q", a, b)
	}
}

func TestURI(t *testing.T) {
	got := URI("Uptime (example.com)", "a+b@x.co", "SECRET")
	for _, want := range []string{
		"otpauth://totp/Uptime%20%28example.com%29:a%2Bb%40x.co?",
		"secret=SECRET",
		"issuer=Uptime%20%28example.com%29",
		"algorithm=SHA1&digits=6&period=30",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("URI %q missing %q", got, want)
		}
	}
	if strings.Contains(got, " ") || strings.Contains(got, "+b") {
		t.Errorf("URI not encoded: %q", got)
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := NewRecoveryCodes(10)
	if err != nil || len(codes) != 10 {
		t.Fatalf("%v %v", codes, err)
	}
	re := regexp.MustCompile(`^[a-z2-7]{4}(-[a-z2-7]{4}){3}$`)
	seen := map[string]bool{}
	for _, c := range codes {
		if !re.MatchString(c) || seen[c] {
			t.Errorf("bad or duplicate code %q", c)
		}
		seen[c] = true
		if NormalizeRecovery(strings.ToUpper(c)) != strings.ReplaceAll(c, "-", "") {
			t.Errorf("normalise %q", c)
		}
	}
	if got := NormalizeRecovery("ABCD efgh-IJKL mnop"); got != "abcdefghijklmnop" {
		t.Errorf("NormalizeRecovery = %q", got)
	}
	h := HashRecovery("abcd-efgh-ijkl-mnop")
	if h != HashRecovery("ABCD EFGH IJKL MNOP") || len(h) != 64 || h == HashRecovery("abcd-efgh-ijkl-mnoq") {
		t.Errorf("hash %q", h)
	}
}
