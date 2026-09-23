package admin

import (
	"sync"
	"time"
)

const (
	MaxLoginFailures   = 5
	LoginFailureWindow = 15 * time.Minute
	maxThrottleTracked = 4096
	throttleSweepEvery = 256
)

type throttle struct {
	mu     sync.Mutex
	counts map[string]window
	writes int
}

type window struct {
	failures int
	until    time.Time
}

func newThrottle() *throttle { return &throttle{counts: make(map[string]window)} }

func (t *throttle) blocked(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	w, ok := t.counts[key]
	return ok && now.Before(w.until) && w.failures >= MaxLoginFailures
}

func (t *throttle) fail(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()

	w := t.counts[key]
	if now.After(w.until) {
		w = window{}
	}
	t.counts[key] = window{failures: w.failures + 1, until: now.Add(LoginFailureWindow)}

	t.writes++
	if t.writes%throttleSweepEvery == 0 || len(t.counts) > maxThrottleTracked {
		t.sweep(now)
	}
}

func (t *throttle) succeed(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.counts, key)
}

func (t *throttle) sweep(now time.Time) {
	for key, w := range t.counts {
		if now.After(w.until) {
			delete(t.counts, key)
		}
	}
}
