package ratelimit

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func newTest(window time.Duration, max, maxKeys int) (*Limiter, *time.Time) {
	now := time.Unix(0, 0)
	l := New(window, max, maxKeys)
	l.Now = func() time.Time { return now }
	return l, &now
}

func TestWindow(t *testing.T) {
	l, now := newTest(time.Minute, 3, 0)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("fresh key must be allowed")
	}
	for _, s := range []int{0, 10, 20} {
		*now = time.Unix(int64(s), 0)
		l.Hit("a")
	}
	*now = time.Unix(30, 0)
	if ok, ra := l.Allow("a"); ok || ra != 30*time.Second {
		t.Fatalf("at 30s: ok=%v retryAfter=%v, want false 30s", ok, ra)
	}
	*now = time.Unix(59, 0).Add(999 * time.Millisecond)
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("at 59.999s must still be blocked")
	}
	*now = time.Unix(60, 0)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("at exactly 60s the 0s event is out of the window")
	}
	l.Hit("a")
	*now = time.Unix(65, 0)
	if ok, ra := l.Allow("a"); ok || ra != 5*time.Second {
		t.Fatalf("at 65s: ok=%v retryAfter=%v, want false 5s", ok, ra)
	}
}

func TestKeysAreIndependentAndReset(t *testing.T) {
	l, _ := newTest(time.Minute, 1, 0)
	l.Hit("a")
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a must be blocked")
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b must not be affected by a")
	}
	l.Reset("a")
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("Reset must unblock a")
	}
}

func TestAllowDoesNotStoreKeys(t *testing.T) {
	l, _ := newTest(time.Minute, 3, 0)
	for i := 0; i < 1000; i++ {
		l.Allow(fmt.Sprint(i))
	}
	if len(l.keys) != 0 {
		t.Fatalf("Allow stored %d keys", len(l.keys))
	}
}

func TestKeyCapEvictsLeastRecentlyUsed(t *testing.T) {
	l, _ := newTest(time.Minute, 1, 2)
	l.Hit("a")
	l.Hit("b")
	l.Allow("a") // touches a, so b is now least recently used
	l.Hit("c")
	if len(l.keys) != 2 {
		t.Fatalf("len(keys) = %d, want 2", len(l.keys))
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("b should have been evicted")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("a should still be blocked")
	}
	if ok, _ := l.Allow("c"); ok {
		t.Fatal("c should be blocked")
	}
}

func TestPerKeyMemoryBounded(t *testing.T) {
	l, _ := newTest(time.Minute, 3, 0)
	for i := 0; i < 100; i++ {
		l.Hit("a")
	}
	if n := len(l.keys["a"].Value.(*entry).times); n != 3 {
		t.Fatalf("stored %d timestamps, want 3", n)
	}
}

func TestConcurrentUse(t *testing.T) {
	l := New(time.Minute, 5, 0)
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				k := fmt.Sprint((g + i) % 10)
				l.Allow(k)
				l.Hit(k)
			}
		}(g)
	}
	wg.Wait()
}

func TestDefaultMaxKeys(t *testing.T) {
	if l := New(time.Minute, 1, 0); l.maxKeys != DefaultMaxKeys {
		t.Fatalf("maxKeys = %d, want %d", l.maxKeys, DefaultMaxKeys)
	}
}

func TestTake(t *testing.T) {
	l, now := newTest(time.Minute, 2, 0)
	for i := 0; i < 2; i++ {
		if ok, _ := l.Take("a"); !ok {
			t.Fatalf("take %d denied", i)
		}
	}
	if ok, ra := l.Take("a"); ok || ra != time.Minute {
		t.Fatalf("third take: ok=%v retryAfter=%v", ok, ra)
	}
	if ok, _ := l.Take("b"); !ok {
		t.Fatal("other key must be allowed")
	}
	*now = time.Unix(61, 0)
	if ok, _ := l.Take("a"); !ok {
		t.Fatal("window passed, take must succeed")
	}
	// A denied take records nothing: one slot frees when the oldest leaves.
	*now = time.Unix(122, 0)
	if ok, _ := l.Take("a"); !ok {
		t.Fatal("denied takes must not extend the block")
	}
}

func TestTakeConcurrent(t *testing.T) {
	l := New(time.Minute, 5, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	n := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Take("k"); ok {
				mu.Lock()
				n++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if n != 5 {
		t.Fatalf("%d takes succeeded, want 5", n)
	}
}
