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
			ID:       sc.ID,
			ChatID:   sc.ChatID,
			Label:    sc.Label,
			Message:  sc.Message,
			Schedule: sc.Schedule,
			Timezone: sc.Timezone,
			Type:     sc.Type,
			Mode:     sc.Mode,
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
