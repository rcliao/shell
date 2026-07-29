package scheduler

import (
	"time"

	"github.com/rcliao/shell/internal/store"
)

// StoreAdapter wraps *store.Store to satisfy ScheduleStore.
type StoreAdapter struct {
	s *store.Store
}

// NewStoreAdapter creates an adapter from a *store.Store.
func NewStoreAdapter(s *store.Store) *StoreAdapter {
	return &StoreAdapter{s: s}
}

func (a *StoreAdapter) GetDueSchedules(now time.Time) ([]ScheduleEntry, error) {
	schedules, err := a.s.GetDueSchedules(now)
	if err != nil {
		return nil, err
	}
	entries := make([]ScheduleEntry, len(schedules))
	for i, sc := range schedules {
		entries[i] = ScheduleEntry{
			ID:        sc.ID,
			ChatID:    sc.ChatID,
			Label:     sc.Label,
			Message:   sc.Message,
			Schedule:  sc.Schedule,
			Timezone:  sc.Timezone,
			Type:      sc.Type,
			Mode:      sc.Mode,
			NextRunAt: sc.NextRunAt,
		}
	}
	return entries, nil
}

func (a *StoreAdapter) UpdateScheduleNextRun(id int64, nextRun time.Time, lastRun time.Time) error {
	return a.s.UpdateScheduleNextRun(id, nextRun, lastRun)
}

func (a *StoreAdapter) DisableSchedule(id int64) error {
	return a.s.DisableSchedule(id)
}

func (a *StoreAdapter) BumpHeartbeatCount(id int64) (int, error) {
	return a.s.BumpHeartbeatCount(id)
}

func (a *StoreAdapter) SetExpectedNextAt(id int64, expected time.Time) error {
	return a.s.SetExpectedNextAt(id, expected)
}

func (a *StoreAdapter) PauseSchedule(id int64, reason string) error {
	return a.s.PauseSchedule(id, reason)
}

func (a *StoreAdapter) RecordJobRun(run JobRun) error {
	rec := store.JobRun{
		ScheduleID:     run.ScheduleID,
		TriggerContext: run.TriggerContext,
		FiredAt:        run.FiredAt,
		Outcome:        run.Outcome,
		ErrorMessage:   run.ErrorMessage,
	}
	if !run.ScheduledAt.IsZero() {
		t := run.ScheduledAt
		rec.ScheduledAt = &t
	}
	return a.s.RecordJobRun(rec)
}

// SilenceChecks builds the dead-man's-switch input from the enabled schedules.
func (a *StoreAdapter) SilenceChecks() ([]SilenceCheck, error) {
	schedules, err := a.s.ListAllSchedules(true)
	if err != nil {
		return nil, err
	}
	checks := make([]SilenceCheck, 0, len(schedules))
	for _, sc := range schedules {
		c := SilenceCheck{
			ID:        sc.ID,
			Label:     sc.Label,
			Type:      sc.Type,
			Schedule:  sc.Schedule,
			Timezone:  sc.Timezone,
			CreatedAt: sc.CreatedAt,
		}
		if sc.LastSuccessAt != nil {
			c.LastSuccessAt = *sc.LastSuccessAt
		}
		checks = append(checks, c)
	}
	return checks, nil
}
