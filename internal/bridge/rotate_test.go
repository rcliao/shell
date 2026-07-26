package bridge

import (
	"testing"
	"time"
)

func TestCalendarDayChanged_SameDayLA(t *testing.T) {
	b := &Bridge{schedulerTZ: "America/Los_Angeles"}
	loc, _ := time.LoadLocation("America/Los_Angeles")

	// Both "today" in LA — early morning vs evening.
	now := time.Now().In(loc)
	startSameDay := time.Date(now.Year(), now.Month(), now.Day(), 1, 0, 0, 0, loc)
	if b.calendarDayChanged(startSameDay) {
		t.Errorf("same-day start should not flag day change")
	}
}

func TestCalendarDayChanged_DifferentDayLA(t *testing.T) {
	b := &Bridge{schedulerTZ: "America/Los_Angeles"}
	loc, _ := time.LoadLocation("America/Los_Angeles")

	// Yesterday at noon, LA-local.
	yesterday := time.Now().In(loc).AddDate(0, 0, -1)
	yesterdayNoon := time.Date(yesterday.Year(), yesterday.Month(), yesterday.Day(), 12, 0, 0, 0, loc)
	if !b.calendarDayChanged(yesterdayNoon) {
		t.Errorf("yesterday start should flag day change")
	}
}

func TestCalendarDayChanged_TZAffectsBoundary(t *testing.T) {
	// 02:00 UTC is "today" in UTC but "yesterday" in LA (which is UTC-7/8).
	// We can't override time.Now in the helper, so verify the underlying
	// math: a start time `nowUTC - 4h` is on a different LA day if and only
	// if local time has crossed midnight in the last 4h.
	loc, _ := time.LoadLocation("America/Los_Angeles")
	nowLA := time.Now().In(loc)

	// If LA time is between 00:00 and 04:00, then 4h ago is yesterday LA.
	// Otherwise same day. Use the helper to verify.
	b := &Bridge{schedulerTZ: "America/Los_Angeles"}
	fourHoursAgo := time.Now().Add(-4 * time.Hour)
	expected := nowLA.Hour() < 4
	if got := b.calendarDayChanged(fourHoursAgo); got != expected {
		t.Errorf("calendarDayChanged(now-4h) = %v, expected %v (LA hour=%d)",
			got, expected, nowLA.Hour())
	}
}

// V2-H45: only fingerprint-triggered pending rotations defer to idle, and only
// while the pending flag is younger than the max-staleness cap.
func TestShouldDeferFingerprintRotation(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		reason    string
		flaggedAt time.Time
		want      bool
	}{
		{"fingerprint fresh flag defers", rotateReasonPromptChanged, now.Add(-5 * time.Minute), true},
		{"fingerprint just under cap defers", rotateReasonPromptChanged, now.Add(-fingerprintDeferMax + time.Second), true},
		{"fingerprint at cap rotates sync", rotateReasonPromptChanged, now.Add(-fingerprintDeferMax), false},
		{"fingerprint stale flag rotates sync", rotateReasonPromptChanged, now.Add(-40 * time.Minute), false},
		{"fingerprint legacy row (zero flagged-at) rotates sync", rotateReasonPromptChanged, time.Time{}, false},
		{"token/cost trigger rotates sync", "cost", now.Add(-5 * time.Minute), false},
		{"latency trigger rotates sync", "latency", now.Add(-5 * time.Minute), false},
		{"pinned_overflow trigger rotates sync", "pinned_overflow", now.Add(-5 * time.Minute), false},
		{"manual trigger rotates sync", "manual", now.Add(-5 * time.Minute), false},
		{"empty reason rotates sync", "", now.Add(-5 * time.Minute), false},
	}
	for _, tc := range cases {
		if got := shouldDeferFingerprintRotation(tc.reason, tc.flaggedAt, now); got != tc.want {
			t.Errorf("%s: shouldDeferFingerprintRotation(%q, age=%s) = %v, want %v",
				tc.name, tc.reason, now.Sub(tc.flaggedAt), got, tc.want)
		}
	}
}

func TestCalendarDayChanged_FallsBackToUTCWhenTZUnset(t *testing.T) {
	b := &Bridge{schedulerTZ: ""}
	yesterdayUTC := time.Now().UTC().AddDate(0, 0, -1)
	if !b.calendarDayChanged(yesterdayUTC) {
		t.Errorf("UTC fallback: yesterday should flag day change")
	}
}
