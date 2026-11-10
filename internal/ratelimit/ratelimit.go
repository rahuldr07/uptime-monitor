// Package ratelimit provides a sliding-window rate limiter keyed by string.
// State is held in memory only and resets when the process restarts.
package ratelimit

import (
	"container/list"
	"sync"
	"time"
)

// DefaultMaxKeys is the key cap used when New is given a maxKeys of 0 or less.
const DefaultMaxKeys = 100_000

// Limiter allows at most max events per key in any sliding window. It is
// safe for concurrent use.
type Limiter struct {
	// Now returns the current time; nil means time.Now. Set it only before
	// first use.
	Now func() time.Time

	window  time.Duration
	max     int
	maxKeys int

	mu    sync.Mutex
	keys  map[string]*list.Element // values are *entry
	order *list.List               // front = most recently used
}

type entry struct {
	key   string
	times []time.Time // oldest first, at most max, all recent enough to matter
}

// New returns a Limiter that allows max events per key within window and
// tracks at most maxKeys keys (DefaultMaxKeys if maxKeys <= 0). A max below 1
// is treated as 1.
func New(window time.Duration, max, maxKeys int) *Limiter {
	if maxKeys <= 0 {
		maxKeys = DefaultMaxKeys
	}
	if max < 1 {
		max = 1
	}
	return &Limiter{window: window, max: max, maxKeys: maxKeys,
		keys: map[string]*list.Element{}, order: list.New()}
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// prune drops events that have left the window (now-window, now]; caller holds mu.
func (l *Limiter) prune(e *entry, now time.Time) {
	cutoff := now.Add(-l.window)
	i := 0
	for i < len(e.times) && !e.times[i].After(cutoff) {
		i++
	}
	if i > 0 {
		e.times = append(e.times[:0], e.times[i:]...)
	}
}

// Allow reports whether key has fewer than max events in the window. When it
// does not, retryAfter is how long until the oldest counted event leaves the
// window. Allow never stores a new key.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, found := l.keys[key]
	if !found {
		return true, 0
	}
	now := l.now()
	e := el.Value.(*entry)
	l.prune(e, now)
	if len(e.times) == 0 {
		l.remove(el)
		return true, 0
	}
	l.order.MoveToFront(el)
	if len(e.times) < l.max {
		return true, 0
	}
	return false, e.times[0].Add(l.window).Sub(now)
}

// Take checks and records in one step: when key has fewer than max events in
// the window it records one and returns true; otherwise it returns false with
// the time until the oldest counted event leaves the window, recording nothing.
func (l *Limiter) Take(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if el, found := l.keys[key]; found {
		e := el.Value.(*entry)
		l.prune(e, now)
		if len(e.times) >= l.max {
			l.order.MoveToFront(el)
			return false, e.times[0].Add(l.window).Sub(now)
		}
	}
	l.record(key, now)
	return true, 0
}

// Hit records one event for key at the current time.
func (l *Limiter) Hit(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record(key, l.now())
}

// record appends an event for key; caller holds mu.
func (l *Limiter) record(key string, now time.Time) {
	el, found := l.keys[key]
	if found {
		l.order.MoveToFront(el)
	} else {
		if len(l.keys) >= l.maxKeys {
			l.remove(l.order.Back())
		}
		el = l.order.PushFront(&entry{key: key})
		l.keys[key] = el
	}
	e := el.Value.(*entry)
	l.prune(e, now)
	e.times = append(e.times, now)
	if len(e.times) > l.max {
		e.times = append(e.times[:0], e.times[len(e.times)-l.max:]...)
	}
}

// Reset forgets key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, found := l.keys[key]; found {
		l.remove(el)
	}
}

// remove deletes el from both indexes; caller holds mu.
func (l *Limiter) remove(el *list.Element) {
	delete(l.keys, el.Value.(*entry).key)
	l.order.Remove(el)
}
