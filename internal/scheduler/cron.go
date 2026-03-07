package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronExpr represents a parsed 5-field cron expression.
type CronExpr struct {
	Minutes    []bool // 0-59
	Hours      []bool // 0-23
	DaysOfMonth []bool // 1-31
	Months     []bool // 1-12
	DaysOfWeek []bool // 0-6 (0=Sunday)
}

// Aliases maps shorthand names to cron expressions.
var aliases = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

// ParseCron parses a 5-field cron expression (minute hour dom month dow).
func ParseCron(expr string) (*CronExpr, error) {
	expr = strings.TrimSpace(expr)
	if alias, ok := aliases[strings.ToLower(expr)]; ok {
		expr = alias
	}

	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d", len(fields))
	}

	c := &CronExpr{
		Minutes:     make([]bool, 60),
		Hours:       make([]bool, 24),
		DaysOfMonth: make([]bool, 32), // index 0 unused
		Months:      make([]bool, 13), // index 0 unused
		DaysOfWeek:  make([]bool, 7),
	}

	if err := parseField(fields[0], c.Minutes, 0, 59); err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	if err := parseField(fields[1], c.Hours, 0, 23); err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	if err := parseField(fields[2], c.DaysOfMonth, 1, 31); err != nil {
		return nil, fmt.Errorf("day-of-month: %w", err)
	}
	if err := parseField(fields[3], c.Months, 1, 12); err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	if err := parseField(fields[4], c.DaysOfWeek, 0, 6); err != nil {
		return nil, fmt.Errorf("day-of-week: %w", err)
	}

	return c, nil
}

// parseField parses a single cron field into a boolean slice.
// Supports: *, */N, N, N-M, N-M/S, and comma-separated lists.
func parseField(field string, bits []bool, min, max int) error {
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if err := parsePart(part, bits, min, max); err != nil {
			return err
		}
	}
	return nil
}

func parsePart(part string, bits []bool, min, max int) error {
	// Handle step: */N or N-M/S
	step := 1
	if idx := strings.Index(part, "/"); idx != -1 {
		s, err := strconv.Atoi(part[idx+1:])
		if err != nil || s <= 0 {
			return fmt.Errorf("invalid step in %q", part)
		}
		step = s
		part = part[:idx]
	}

	var lo, hi int
	switch {
	case part == "*":
		lo, hi = min, max
	case strings.Contains(part, "-"):
		rng := strings.SplitN(part, "-", 2)
		var err error
		lo, err = strconv.Atoi(rng[0])
		if err != nil {
			return fmt.Errorf("invalid range start in %q", part)
		}
		hi, err = strconv.Atoi(rng[1])
		if err != nil {
			return fmt.Errorf("invalid range end in %q", part)
		}
	default:
		v, err := strconv.Atoi(part)
		if err != nil {
			return fmt.Errorf("invalid value %q", part)
		}
		lo, hi = v, v
	}

	if lo < min || hi > max || lo > hi {
		return fmt.Errorf("value out of range [%d-%d]: %d-%d", min, max, lo, hi)
	}

	for i := lo; i <= hi; i += step {
		bits[i] = true
	}
	return nil
}

// Next returns the next time after `after` that matches the cron expression.
// It searches up to 4 years ahead to avoid infinite loops.
func (c *CronExpr) Next(after time.Time) time.Time {
	// Start from the next minute
	t := after.Truncate(time.Minute).Add(time.Minute)

	// Search limit: ~4 years
	limit := after.Add(4 * 365 * 24 * time.Hour)

	for t.Before(limit) {
		if !c.Months[t.Month()] {
			// Advance to next month
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.DaysOfMonth[t.Day()] || !c.DaysOfWeek[t.Weekday()] {
			// Advance to next day
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.Hours[t.Hour()] {
			// Advance to next hour
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !c.Minutes[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}

	return time.Time{} // zero value if nothing found
}
