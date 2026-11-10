package password

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var fast = Params{Memory: 64, Time: 1, Threads: 1}

func TestRoundTrip(t *testing.T) {
	h := Default()
	phc, err := h.Hash(context.Background(), "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", phc)
	}
	ok, err := h.Verify(context.Background(), phc, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("Verify right pw = %v, %v", ok, err)
	}
	ok, err = h.Verify(context.Background(), phc, "wrong horse battery")
	if err != nil || ok {
		t.Fatalf("Verify wrong pw = %v, %v", ok, err)
	}
}

func TestVerifyParsesParams(t *testing.T) {
	phc, err := New(fast).Hash(context.Background(), "some password")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Default().Verify(context.Background(), phc, "some password")
	if err != nil || !ok {
		t.Fatalf("Verify = %v, %v", ok, err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	h := New(fast)
	good, err := h.Hash(context.Background(), "pw")
	if err != nil {
		t.Fatal(err)
	}
	for _, phc := range []string{
		"",
		"$bcrypt$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA",
		good[:len(good)/2],
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=x,t=1,p=1$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$!!$aGFzaA",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdA$",
		good + "\n",
		strings.Replace(good, "v=19", "v=19junk", 1),
		strings.Replace(good, "m=64,t=1,p=1", "m=64,t=1,p=1,x=1", 1),
		strings.Replace(good, "m=64", "m=4194304", 1),
		strings.Replace(good, "t=1", "t=100", 1),
		strings.Replace(good, "p=1", "p=0", 1),
		strings.Replace(good, "p=1", "p=8", 1),
		"$argon2id$v=19$m=64,t=1,p=1$AAAAAAAAAAAAAAAAAAAA$" + parts32(good),
		"$argon2id$v=19$m=64,t=1,p=1$" + strings.Split(good, "$")[4] + "$YQ",
	} {
		ok, err := h.Verify(context.Background(), phc, "pw")
		if ok || err == nil {
			t.Errorf("Verify(%q) = %v, %v; want false with error", phc, ok, err)
		}
	}
}

func parts32(phc string) string { return strings.Split(phc, "$")[5] }

func TestLengthGuard(t *testing.T) {
	h := New(fast)
	long := strings.Repeat("a", 257)
	if _, err := h.Hash(context.Background(), long); err != ErrTooLong {
		t.Errorf("Hash: %v", err)
	}
	if ok, err := h.Verify(context.Background(), h.dummy, long); ok || err != ErrTooLong {
		t.Errorf("Verify: %v, %v", ok, err)
	}
}

func TestVerifyDummy(t *testing.T) {
	h := New(Params{Memory: 4096, Time: 2, Threads: 1})
	phc, _ := h.Hash(context.Background(), "pw")
	s := time.Now()
	if _, err := h.Verify(context.Background(), phc, "pw"); err != nil {
		t.Fatal(err)
	}
	vd := time.Since(s)
	s = time.Now()
	if err := h.VerifyDummy(context.Background()); err != nil {
		t.Fatalf("VerifyDummy = %v", err)
	}
	if dd := time.Since(s); dd < vd/4 || dd > vd*4 {
		t.Errorf("VerifyDummy took %v, Verify %v", dd, vd)
	}

	// Saturated: returns ErrBusy like Verify would.
	h.wait = 20 * time.Millisecond
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h.hashFn = func(pw, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		started <- struct{}{}
		<-release
		return make([]byte, keyLen)
	}
	for i := 0; i < 4; i++ {
		go h.Hash(context.Background(), "pw")
	}
	for i := 0; i < 4; i++ {
		<-started
	}
	if err := h.VerifyDummy(context.Background()); !errors.Is(err, ErrBusy) {
		t.Errorf("saturated VerifyDummy = %v, want ErrBusy", err)
	}
	close(release)
}

func TestCheckLength(t *testing.T) {
	if err := CheckLength("123456789"); err != ErrTooShort {
		t.Errorf("9 chars: %v", err)
	}
	if err := CheckLength("1234567890"); err != nil {
		t.Errorf("10 chars: %v", err)
	}
	if err := CheckLength(strings.Repeat("é", 10)); err != nil {
		t.Errorf("10 multi-byte runes: %v", err)
	}
	if err := CheckLength(strings.Repeat("a", 256)); err != nil {
		t.Errorf("256 bytes: %v", err)
	}
	if err := CheckLength(strings.Repeat("a", 257)); err != ErrTooLong {
		t.Errorf("257 bytes: %v", err)
	}
	if ErrTooShort.Error() != "Use at least 10 characters." || ErrTooLong.Error() != "Use at most 256 bytes." {
		t.Error("error messages differ from spec")
	}
}

func TestBusy(t *testing.T) {
	h := New(fast)
	h.wait = 50 * time.Millisecond
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	h.hashFn = func(pw, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte {
		started <- struct{}{}
		<-release
		return make([]byte, keyLen)
	}
	bg := context.Background()
	done := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() { _, err := h.Hash(bg, "pw"); done <- err }()
	}
	for i := 0; i < 4; i++ {
		<-started
	}

	// 5th caller waits, then times out.
	begin := time.Now()
	if _, err := h.Hash(bg, "pw"); !errors.Is(err, ErrBusy) {
		t.Fatalf("5th Hash err = %v, want ErrBusy", err)
	}
	if d := time.Since(begin); d < 40*time.Millisecond {
		t.Errorf("ErrBusy after %v, want about 50ms", d)
	}

	// Cancelled ctx while waiting.
	ctx, cancel := context.WithCancel(bg)
	errc := make(chan error, 1)
	h.wait = time.Minute
	go func() { _, err := h.Hash(ctx, "pw"); errc <- err }()
	for h.waiters.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v, want context.Canceled", err)
	}

	// Fill the wait queue; the next caller is rejected at once.
	wctx, wcancel := context.WithCancel(bg)
	werrs := make(chan error, maxWaiters)
	for i := 0; i < maxWaiters; i++ {
		go func() { _, err := h.Hash(wctx, "pw"); werrs <- err }()
	}
	for h.waiters.Load() != maxWaiters {
		time.Sleep(time.Millisecond)
	}
	begin = time.Now()
	if _, err := h.Hash(bg, "pw"); !errors.Is(err, ErrBusy) {
		t.Fatalf("65th Hash err = %v, want ErrBusy", err)
	}
	if d := time.Since(begin); d > 40*time.Millisecond {
		t.Errorf("full queue took %v, want immediate", d)
	}
	wcancel()
	for i := 0; i < maxWaiters; i++ {
		if err := <-werrs; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter err = %v", err)
		}
	}
	close(release)
	for i := 0; i < 4; i++ {
		if err := <-done; err != nil {
			t.Errorf("running hash err = %v", err)
		}
	}
	if n := len(h.sem); n != 0 {
		t.Errorf("sem not drained: %d", n)
	}
	if n := h.waiters.Load(); n != 0 {
		t.Errorf("waiters not zero: %d", n)
	}
}
