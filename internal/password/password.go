// Package password hashes and verifies passwords with argon2id. Hashing is
// memory-hungry, so concurrent runs are bounded: at most maxRunning at once,
// at most maxWaiters queued, and a queued caller waits at most Hasher.wait.
package password

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

const (
	maxRunning = 4
	maxWaiters = 64
	saltLen    = 16
	keyLen     = 32
	minRunes   = 10
	maxBytes   = 256
)

var (
	// ErrBusy means the hasher is saturated; the caller should answer 503.
	ErrBusy = errors.New("password: server busy")
	// ErrTooShort and ErrTooLong are shown to users as-is.
	ErrTooShort = errors.New("Use at least 10 characters.")
	ErrTooLong  = errors.New("Use at most 256 bytes.")

	errBadHash = errors.New("password: invalid hash")
)

// Params are the argon2id cost parameters.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
}

// DefaultParams is the OWASP-recommended argon2id minimum (19 MiB, t=2, p=1).
var DefaultParams = Params{Memory: 19456, Time: 2, Threads: 1}

// Hasher hashes and verifies passwords under a concurrency bound.
type Hasher struct {
	params  Params
	sem     chan struct{}
	waiters atomic.Int32
	wait    time.Duration
	hashFn  func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte
	dummy   string
}

// New returns a Hasher using p for new hashes.
func New(p Params) *Hasher {
	h := &Hasher{
		params: p,
		sem:    make(chan struct{}, maxRunning),
		wait:   2 * time.Second,
		hashFn: argon2.IDKey,
	}
	h.dummy = h.encode([]byte("dummy password"), make([]byte, saltLen))
	return h
}

var defaultHasher = sync.OnceValue(func() *Hasher { return New(DefaultParams) })

// Default returns the process-wide Hasher using DefaultParams.
func Default() *Hasher { return defaultHasher() }

// acquire takes a run slot, or fails with ErrBusy or the context's error.
func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	default:
	}
	if h.waiters.Add(1) > maxWaiters {
		h.waiters.Add(-1)
		return ErrBusy
	}
	defer h.waiters.Add(-1)
	timer := time.NewTimer(h.wait)
	defer timer.Stop()
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-timer.C:
		return ErrBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

func (h *Hasher) encode(pw, salt []byte) string {
	p := h.params
	key := h.hashFn(pw, salt, p.Time, p.Memory, p.Threads, keyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// Hash returns a PHC-format argon2id hash of pw with a fresh random salt.
func (h *Hasher) Hash(ctx context.Context, pw string) (string, error) {
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	defer h.release()
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return h.encode([]byte(pw), salt), nil
}

// Verify reports whether pw matches phc. The cost parameters come from phc,
// so hashes made under older parameters keep working.
func (h *Hasher) Verify(ctx context.Context, phc, pw string) (bool, error) {
	var version int
	var p Params
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errBadHash
	}
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil {
		return false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || p.Memory == 0 || p.Time == 0 || p.Threads == 0 {
		return false, errBadHash
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	defer h.release()
	got := h.hashFn([]byte(pw), salt, p.Time, p.Memory, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// VerifyDummy burns the cost of one Verify against a fixed hash, so unknown
// emails take as long as known ones. The result and any error are ignored.
func (h *Hasher) VerifyDummy(ctx context.Context) {
	_, _ = h.Verify(ctx, h.dummy, "wrong password")
}

// CheckLength enforces the password length rule: at least 10 runes, at most
// 256 bytes.
func CheckLength(pw string) error {
	if utf8.RuneCountInString(pw) < minRunes {
		return ErrTooShort
	}
	if len(pw) > maxBytes {
		return ErrTooLong
	}
	return nil
}
