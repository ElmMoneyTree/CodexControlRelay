package server

import (
	"sync"
	"time"

	"codex-control/relay-go/internal/config"
)

type rateLimiter struct {
	mu      sync.Mutex
	limit   config.RateLimit
	entries map[string]rateEntry
	lastGC  time.Time
}

type rateEntry struct {
	windowStart time.Time
	count       int
}

func newRateLimiter(limit config.RateLimit) *rateLimiter {
	return &rateLimiter{limit: limit, entries: map[string]rateEntry{}, lastGC: time.Now()}
}

func (l *rateLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.entries[key]
	if entry.windowStart.IsZero() || now.Sub(entry.windowStart) >= l.limit.Window.Duration {
		entry = rateEntry{windowStart: now, count: 0}
	}
	entry.count++
	l.entries[key] = entry
	if now.Sub(l.lastGC) >= l.limit.Window.Duration {
		for candidate, value := range l.entries {
			if now.Sub(value.windowStart) >= 2*l.limit.Window.Duration {
				delete(l.entries, candidate)
			}
		}
		l.lastGC = now
	}
	remaining := l.limit.Window.Duration - now.Sub(entry.windowStart)
	return entry.count <= l.limit.Requests, remaining
}
