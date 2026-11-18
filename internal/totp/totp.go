// Package totp implements RFC 6238 time-based one-time passwords (HMAC-SHA1,
// 6 digits, 30-second steps) and single-use recovery codes.
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	period = 30
	digits = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns 20 random bytes as upper-case base32 without padding.
func NewSecret() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// Step is the 30-second time step containing t.
func Step(t time.Time) int64 { return t.Unix() / period }

// Code returns the 6-digit code of secret (base32) for a time step.
func Code(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", fmt.Errorf("totp: bad secret: %w", err)
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	n := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", digits, n%1000000), nil
}

// Match checks code against the steps before, at and after now's, comparing
// every candidate in constant time. It returns the matched step; the caller
// must still reject a replayed step atomically.
func Match(secret, code string, now time.Time) (step int64, ok bool) {
	cur := Step(now)
	for s := cur - 1; s <= cur+1; s++ {
		want, err := Code(secret, s)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			// Keep scanning so timing does not reveal which step matched.
			step, ok = s, true
		}
	}
	return step, ok
}

func escape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// URI is the otpauth:// provisioning URI for authenticator apps.
func URI(issuer, email, secret string) string {
	return "otpauth://totp/" + escape(issuer) + ":" + escape(email) +
		"?secret=" + secret + "&issuer=" + escape(issuer) +
		"&algorithm=SHA1&digits=6&period=30"
}

// NewRecoveryCodes returns n codes shaped xxxx-xxxx-xxxx-xxxx (80 bits of
// lower-case base32).
func NewRecoveryCodes(n int) ([]string, error) {
	out := make([]string, 0, n)
	for len(out) < n {
		b := make([]byte, 10)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		s := strings.ToLower(b32.EncodeToString(b)) // 16 characters
		out = append(out, s[0:4]+"-"+s[4:8]+"-"+s[8:12]+"-"+s[12:16])
	}
	return out, nil
}

// NormalizeRecovery lower-cases in and drops spaces and dashes.
func NormalizeRecovery(in string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '\t', '\n', '\r':
			return -1
		}
		return r
	}, strings.ToLower(in))
}

// HashRecovery is the hex SHA-256 of the normalised code, which is what is stored.
func HashRecovery(code string) string {
	sum := sha256.Sum256([]byte(NormalizeRecovery(code)))
	return hex.EncodeToString(sum[:])
}
