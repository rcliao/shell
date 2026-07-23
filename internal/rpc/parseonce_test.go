package rpc

import (
	"strings"
	"testing"
	"time"
)

// V2-H49: the /schedule once-path rejected the formats agents actually send
// (bare "21:00", space-separated datetimes) with an unactionable 400, so
// reminders were silently never created. parseOnceAt accepts those formats
// and rejects past times with a message the agent can act on.
func TestParseOnceAt(t *testing.T) {
	la, _ := time.LoadLocation("America/Los_Angeles")
	// Fixed "now": 2026-07-23 10:00 PDT.
	now := time.Date(2026, 7, 23, 10, 0, 0, 0, la)

	t.Run("rfc3339", func(t *testing.T) {
		got, err := parseOnceAt("2026-07-23T21:00:00-07:00", la, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hour() != 21 {
			t.Errorf("hour = %d", got.Hour())
		}
	})

	t.Run("iso-t no tz", func(t *testing.T) {
		if _, err := parseOnceAt("2026-07-23T21:00:00", la, now); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("space-separated datetime", func(t *testing.T) {
		if _, err := parseOnceAt("2026-07-23 21:00", la, now); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("bare clock time ahead resolves to today", func(t *testing.T) {
		got, err := parseOnceAt("21:00", la, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Day() != 23 || got.Hour() != 21 {
			t.Errorf("got %v, want today 21:00", got)
		}
	})

	t.Run("bare clock time already passed resolves to tomorrow", func(t *testing.T) {
		got, err := parseOnceAt("09:00", la, now)
		if err != nil {
			t.Fatal(err)
		}
		if got.Day() != 24 || got.Hour() != 9 {
			t.Errorf("got %v, want tomorrow 09:00", got)
		}
	})

	t.Run("past datetime rejected with actionable message", func(t *testing.T) {
		_, err := parseOnceAt("2026-07-23 08:00", la, now)
		if err == nil {
			t.Fatal("want error for past time")
		}
		if !strings.Contains(err.Error(), "resolves to the past") {
			t.Errorf("unactionable error: %v", err)
		}
	})

	t.Run("garbage rejected with format list", func(t *testing.T) {
		_, err := parseOnceAt("tonight", la, now)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), "accepted formats") {
			t.Errorf("unactionable error: %v", err)
		}
	})

	t.Run("empty at", func(t *testing.T) {
		if _, err := parseOnceAt("", la, now); err == nil {
			t.Fatal("want error for empty at")
		}
	})
}
