package scheduler

import (
	"math/rand"
	"sync"
	"time"
)

// Firing jitter (V3-T1).
//
// Two agents, their heartbeats, and every watch job all land on :00, which
// self-inflicts a Telegram rate-limit spike and rotation thrash every hour.
// Jitter spreads the FIRING MOMENT only: the cron next-run computation stays
// exact (schedules.expected_next_at keeps the true occurrence), and a jittered
// fire may never slide past the following occurrence.
//
// Determinism: the fraction comes from an injectable source, so tests pin it
// instead of depending on wall-clock randomness.

const (
	// jitterFraction is the share of a schedule's interval available for jitter.
	jitterFraction = 0.15
	// jitterCap bounds jitter for long intervals — a daily job must not drift
	// by hours.
	jitterCap = 9 * time.Minute
	// jitterFloor is the minimum jitter window, so even one-shots and very
	// short intervals get a few seconds of spread.
	jitterFloor = 5 * time.Second
)

// JitterFracFunc returns a fraction in [0,1) used to scale the jitter window.
// Injectable so tests are deterministic.
type JitterFracFunc func(scheduleID int64) float64

// NewSeededJitter returns a JitterFracFunc backed by a seeded PRNG. Callers
// that need reproducible output pass a fixed seed; the daemon seeds from the
// clock at startup.
func NewSeededJitter(seed int64) JitterFracFunc {
	var mu sync.Mutex
	r := rand.New(rand.NewSource(seed))
	return func(int64) float64 {
		mu.Lock()
		defer mu.Unlock()
		return r.Float64()
	}
}

// jitterWindow returns the maximum jitter for a schedule interval:
// min(15% of interval, 9min), floored at 5s.
func jitterWindow(interval time.Duration) time.Duration {
	if interval < 0 {
		interval = 0
	}
	w := time.Duration(float64(interval) * jitterFraction)
	if w > jitterCap {
		w = jitterCap
	}
	if w < jitterFloor {
		w = jitterFloor
	}
	return w
}

// applyJitter delays an exact occurrence by frac × jitterWindow(interval).
//
// following is the occurrence AFTER base (zero when unknown or when the
// schedule fires only once). The jittered moment is clamped to strictly before
// it, so jitter can never swallow or reorder the next fire.
func applyJitter(base time.Time, interval time.Duration, following time.Time, frac float64) time.Time {
	if frac < 0 {
		frac = 0
	}
	if frac >= 1 {
		frac = 0.999999
	}
	delay := time.Duration(float64(jitterWindow(interval)) * frac)
	if delay <= 0 {
		return base
	}
	fire := base.Add(delay)
	if !following.IsZero() && !fire.Before(following) {
		// Land just short of the next occurrence rather than on/after it.
		fire = following.Add(-time.Second)
		if fire.Before(base) {
			return base
		}
	}
	return fire
}

// jitterFor returns the scheduler's jitter fraction for a schedule, or 0 when
// jitter is disabled (nil source).
func (s *Scheduler) jitterFor(id int64) float64 {
	if s.jitterFrac == nil {
		return 0
	}
	return s.jitterFrac(id)
}
