package telegram

import (
	"sync"
	"time"
)

// RateLimiter tracks denied/pairing attempts per sender using a sliding window.
type RateLimiter struct {
	mu     sync.Mutex
	events map[int64][]time.Time
	limit  int
	window time.Duration
}

// NewRateLimiter creates a rate limiter that allows at most limit attempts per window.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		events: make(map[int64][]time.Time),
		limit:  limit,
		window: window,
	}
}

// Allow returns true if the user hasn't exceeded the rate limit.
// Records the attempt. Must only be called for denied/pairing users.
func (r *RateLimiter) Allow(userID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-r.window)

	// Prune old events for this user.
	events := r.events[userID]
	start := 0
	for start < len(events) && events[start].Before(cutoff) {
		start++
	}
	events = events[start:]

	if len(events) >= r.limit {
		r.events[userID] = events
		return false
	}

	r.events[userID] = append(events, now)
	return true
}

// Cleanup removes stale entries older than the window.
func (r *RateLimiter) Cleanup() {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := time.Now().Add(-r.window)
	for uid, events := range r.events {
		start := 0
		for start < len(events) && events[start].Before(cutoff) {
			start++
		}
		if start >= len(events) {
			delete(r.events, uid)
		} else {
			r.events[uid] = events[start:]
		}
	}
}
