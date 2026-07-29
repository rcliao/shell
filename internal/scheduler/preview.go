package scheduler

import (
	"fmt"
	"log/slog"
	"time"
)

// Schedule preview + dead-man's-switch (V3-T1).
//
// Preview automates the standing "verify against the DB after creating"
// rule: creation and listing both report the next fire times, already resolved
// in the schedule's timezone, so a wrong cron expression is visible
// immediately instead of at the first missed reminder.
//
// The dead-man's-switch catches the opposite failure — SILENCE. A schedule
// whose last SUCCESSFUL run is older than interval × 2 is either not firing or
// failing before it reaches the chat, and neither shows up in failure-rate
// metrics.

// silenceMultiplier is how many intervals of quiet count as "this schedule is
// dead". Two full intervals tolerates one skipped occurrence (a restart, a
// quiet-hours push) without crying wolf.
const silenceMultiplier = 2

// LoadLocationOrUTC resolves a timezone name, falling back to UTC.
func LoadLocationOrUTC(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.UTC
	}
	return loc
}

// NextRuns returns the next n fire times for a schedule, resolved in the
// schedule's timezone. One-shots return their single remaining occurrence.
// Times are exact occurrences — jitter is a firing-moment detail and is
// deliberately not shown.
func NextRuns(schedType, expr, tz string, next time.Time, from time.Time, n int) ([]time.Time, error) {
	if n <= 0 {
		n = 3
	}
	loc := LoadLocationOrUTC(tz)

	switch schedType {
	case "once":
		if next.IsZero() || next.Before(from) {
			return nil, nil
		}
		return []time.Time{next.In(loc)}, nil

	case "heartbeat":
		interval, err := time.ParseDuration(expr)
		if err != nil || interval <= 0 {
			return nil, fmt.Errorf("invalid heartbeat interval %q", expr)
		}
		base := next
		if base.IsZero() || base.Before(from) {
			base = from.Add(interval)
		}
		out := make([]time.Time, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, base.In(loc))
			base = base.Add(interval)
		}
		return out, nil

	default: // "cron"
		cron, err := ParseCron(expr)
		if err != nil {
			return nil, fmt.Errorf("invalid cron %q: %w", expr, err)
		}
		t := from.In(loc)
		out := make([]time.Time, 0, n)
		for i := 0; i < n; i++ {
			t = cron.Next(t)
			if t.IsZero() {
				break
			}
			out = append(out, t)
		}
		return out, nil
	}
}

// intervalProbeOccurrences is how many upcoming occurrences ScheduleInterval
// samples. Irregular expressions ("30 11 * * 0,6" — weekend mornings) have
// wildly different consecutive gaps; taking the LARGEST keeps the
// dead-man's-switch from crying wolf across the long gap.
const intervalProbeOccurrences = 5

// ScheduleInterval returns the widest gap between consecutive occurrences of a
// schedule, sampled over the next few occurrences from `from`. One-shots have
// no interval (0).
func ScheduleInterval(schedType, expr, tz string, from time.Time) (time.Duration, error) {
	switch schedType {
	case "once":
		return 0, nil
	case "heartbeat":
		d, err := time.ParseDuration(expr)
		if err != nil {
			return 0, fmt.Errorf("invalid heartbeat interval %q", expr)
		}
		return d, nil
	default:
		cron, err := ParseCron(expr)
		if err != nil {
			return 0, fmt.Errorf("invalid cron %q: %w", expr, err)
		}
		loc := LoadLocationOrUTC(tz)
		prev := cron.Next(from.In(loc))
		if prev.IsZero() {
			return 0, fmt.Errorf("cron %q has no next occurrence", expr)
		}
		var widest time.Duration
		for i := 0; i < intervalProbeOccurrences; i++ {
			next := cron.Next(prev)
			if next.IsZero() {
				break
			}
			if gap := next.Sub(prev); gap > widest {
				widest = gap
			}
			prev = next
		}
		return widest, nil
	}
}

// SilenceCheck is the per-schedule input to the dead-man's-switch.
type SilenceCheck struct {
	ID            int64
	Label         string
	Type          string
	Schedule      string
	Timezone      string
	LastSuccessAt time.Time // zero = never succeeded
	CreatedAt     time.Time // fallback reference when never succeeded
}

// SilenceReport describes one schedule that has gone quiet.
type SilenceReport struct {
	ID        int64
	Label     string
	Silent    time.Duration // how long since the last successful run
	Threshold time.Duration // interval × silenceMultiplier
}

// DetectSilentSchedules returns the enabled schedules whose last SUCCESSFUL run
// is older than interval × 2. One-shots and schedules with unparseable
// expressions are skipped — the latter are handled by auto-pause, which records
// a paused_reason instead.
func DetectSilentSchedules(checks []SilenceCheck, now time.Time) []SilenceReport {
	var out []SilenceReport
	for _, c := range checks {
		// Heartbeats are deliberately exempt: quiet-hours suppression and idle
		// backoff both make long legitimate gaps, so their declared interval is
		// not their expected cadence and every quiet night would look like a
		// death. They come under the switch when the heartbeat becomes a
		// system-owned cron row (a later sprint) and its real cadence is a
		// property of the row rather than of two runtime knobs.
		if c.Type == "heartbeat" {
			continue
		}
		interval, err := ScheduleInterval(c.Type, c.Schedule, c.Timezone, now)
		if err != nil || interval <= 0 {
			continue
		}
		ref := c.LastSuccessAt
		if ref.IsZero() {
			ref = c.CreatedAt
		}
		if ref.IsZero() {
			continue
		}
		threshold := time.Duration(silenceMultiplier) * interval
		if silent := now.Sub(ref); silent > threshold {
			out = append(out, SilenceReport{ID: c.ID, Label: c.Label, Silent: silent, Threshold: threshold})
		}
	}
	return out
}

// LogSilentSchedules runs the dead-man's-switch and logs a loud WARN per silent
// schedule. Called from the daemon's existing maintenance ticker — no extra
// goroutine.
func LogSilentSchedules(checks []SilenceCheck, now time.Time) int {
	reports := DetectSilentSchedules(checks, now)
	for _, r := range reports {
		slog.Warn("schedule silent",
			"id", r.ID,
			"label", r.Label,
			"silent_for", r.Silent.Round(time.Minute),
			"threshold", r.Threshold.Round(time.Minute),
			"hint", "last successful run is older than interval x2 — check job_runs (`shell job-runs --schedule <id>`)",
		)
	}
	return len(reports)
}
