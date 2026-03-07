package scheduler

import (
	"testing"
	"time"
)

func TestParseCron(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"every minute", "* * * * *", false},
		{"every 5 min", "*/5 * * * *", false},
		{"specific time", "30 9 * * *", false},
		{"weekdays 9am", "0 9 * * 1-5", false},
		{"monthly", "0 0 1 * *", false},
		{"comma list", "0,15,30,45 * * * *", false},
		{"alias hourly", "@hourly", false},
		{"alias daily", "@daily", false},
		{"alias weekly", "@weekly", false},
		{"alias monthly", "@monthly", false},
		{"too few fields", "* * *", true},
		{"too many fields", "* * * * * *", true},
		{"out of range min", "60 * * * *", true},
		{"out of range hour", "* 25 * * *", true},
		{"invalid", "abc * * * *", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseCron(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCron(%q) error = %v, wantErr %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}

func TestCronNext(t *testing.T) {
	loc := time.UTC
	base := time.Date(2026, 3, 5, 10, 30, 0, 0, loc)

	tests := []struct {
		name string
		expr string
		want time.Time
	}{
		{
			"every minute",
			"* * * * *",
			time.Date(2026, 3, 5, 10, 31, 0, 0, loc),
		},
		{
			"next hour",
			"0 * * * *",
			time.Date(2026, 3, 5, 11, 0, 0, 0, loc),
		},
		{
			"specific time tomorrow",
			"0 9 * * *",
			time.Date(2026, 3, 6, 9, 0, 0, 0, loc),
		},
		{
			"every 15 min",
			"*/15 * * * *",
			time.Date(2026, 3, 5, 10, 45, 0, 0, loc),
		},
		{
			"weekdays only from Thursday",
			"0 9 * * 1-5",
			time.Date(2026, 3, 6, 9, 0, 0, 0, loc), // 2026-03-05 is Thursday, next is Friday
		},
		{
			"first of month",
			"0 0 1 * *",
			time.Date(2026, 4, 1, 0, 0, 0, 0, loc),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ParseCron(tt.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q) = %v", tt.expr, err)
			}
			got := c.Next(base)
			if !got.Equal(tt.want) {
				t.Errorf("Next(%v) = %v, want %v", base, got, tt.want)
			}
		})
	}
}
