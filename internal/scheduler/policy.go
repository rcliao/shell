package scheduler

import (
	"strings"
	"time"
)

// Overlap and retry policy.
//
// Vocabulary borrowed from Temporal Schedules, which solved this interface
// before us. Deliberately NOT borrowed: durable/event-sourced execution and
// backfill. This is a single-node family deployment, and replaying missed
// occurrences is a documented non-goal — a schedule paused for a week must not
// fire seven times on resume.

// OverlapPolicy decides what happens when a schedule comes due while its
// previous run is still in flight.
type OverlapPolicy string

const (
	// OverlapSkip drops the new fire. Right for heartbeats: a missed beat is
	// invisible, a doubled one is noise in the family chat.
	OverlapSkip OverlapPolicy = "skip"
	// OverlapBufferOne runs the new fire after the current one finishes, with
	// at most one waiting. Right for user-facing reminders, where the point is
	// that the message eventually arrives.
	OverlapBufferOne OverlapPolicy = "buffer_one"
	// OverlapAllow starts concurrently. Available but never the default — for
	// this system it means two turns racing for one Claude subprocess.
	OverlapAllow OverlapPolicy = "allow"
)

// ParseOverlapPolicy resolves a stored policy string, falling back to the
// type-appropriate default when unset or unrecognized.
func ParseOverlapPolicy(s, scheduleType string) OverlapPolicy {
	switch OverlapPolicy(strings.TrimSpace(strings.ToLower(s))) {
	case OverlapSkip:
		return OverlapSkip
	case OverlapBufferOne:
		return OverlapBufferOne
	case OverlapAllow:
		return OverlapAllow
	}
	return DefaultOverlapPolicy(scheduleType)
}

// DefaultOverlapPolicy is skip for heartbeats, buffer_one for everything else.
func DefaultOverlapPolicy(scheduleType string) OverlapPolicy {
	if scheduleType == "heartbeat" {
		return OverlapSkip
	}
	return OverlapBufferOne
}

// RetryPolicy bounds re-attempts of a failed run.
type RetryPolicy struct {
	MaxAttempts     int           // total attempts including the first
	InitialInterval time.Duration // wait before attempt 2
	BackoffCoeff    float64       // multiplier per subsequent attempt
	MaxInterval     time.Duration // ceiling on the wait
}

// DefaultRetryPolicy retries a failed turn three times over roughly two
// minutes. Kept short on purpose: a scheduled job that lands twenty minutes
// late is often worse than one that doesn't land, and the next occurrence is
// usually not far away.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     3,
		InitialInterval: 15 * time.Second,
		BackoffCoeff:    3,
		MaxInterval:     90 * time.Second,
	}
}

// Backoff returns how long to wait before the given attempt number (2-based:
// Backoff(2) is the wait after the first failure).
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	d := p.InitialInterval
	for i := 2; i < attempt; i++ {
		d = time.Duration(float64(d) * p.BackoffCoeff)
		if d >= p.MaxInterval {
			return p.MaxInterval
		}
	}
	if d > p.MaxInterval {
		d = p.MaxInterval
	}
	return d
}

// retryableFragments are substrings of errors worth trying again. The list is
// deliberately narrow: retrying a genuine bug wastes a turn and can double a
// side effect that already happened. Anything not matched here fails fast.
var retryableFragments = []string{
	"401",             // expired OAuth token — the CLI reauths on respawn
	"unauthorized",    //
	"429",             // rate limited
	"rate limit",      //
	"overloaded",      // upstream capacity
	"503",             //
	"502",             //
	"timeout",         // slow turn, not a wrong one
	"deadline exceed", //
	"connection reset",
	"broken pipe",
	"eof", // subprocess died mid-stream
	"session busy",
	"user busy",
}

// IsRetryable reports whether an error looks transient. Conservative by design:
// unmatched errors are treated as permanent.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, frag := range retryableFragments {
		if strings.Contains(msg, frag) {
			return true
		}
	}
	return false
}
