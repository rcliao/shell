package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// ScheduleStore is the interface the scheduler needs from the store.
type ScheduleStore interface {
	GetDueSchedules(now time.Time) ([]ScheduleEntry, error)
	UpdateScheduleNextRun(id int64, nextRun time.Time, lastRun time.Time) error
	DisableSchedule(id int64) error
}

// ScheduleEntry mirrors store.Schedule but avoids a circular import.
type ScheduleEntry struct {
	ID       int64
	ChatID   int64
	Label    string
	Message  string
	Schedule string // cron expr or ISO8601
	Timezone string
	Type     string // "cron" or "once"
	Mode     string // "notify" or "prompt"
}

// NotifyFunc sends a plain text message to a chat.
type NotifyFunc func(chatID int64, msg string)

// PromptFunc routes a message through Claude as if the user sent it.
type PromptFunc func(chatID int64, msg string)

// Scheduler runs a 1-minute tick loop to fire due schedules.
type Scheduler struct {
	store     ScheduleStore
	onNotify  NotifyFunc
	onPrompt  PromptFunc
	defaultTZ string
}

// New creates a new Scheduler.
func New(store ScheduleStore, onNotify NotifyFunc, onPrompt PromptFunc, defaultTZ string) *Scheduler {
	if defaultTZ == "" {
		defaultTZ = "UTC"
	}
	return &Scheduler{
		store:     store,
		onNotify:  onNotify,
		onPrompt:  onPrompt,
		defaultTZ: defaultTZ,
	}
}

// Run starts the scheduler tick loop. Blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	slog.Info("scheduler started")
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Run immediately on start, then on each tick
	s.tick()

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler stopped")
			return
		case <-ticker.C:
			s.tick()
		}
	}
}

func (s *Scheduler) tick() {
	now := time.Now().UTC()
	schedules, err := s.store.GetDueSchedules(now)
	if err != nil {
		slog.Error("scheduler: failed to get due schedules", "error", err)
		return
	}

	for _, sc := range schedules {
		s.execute(sc)
		s.advance(sc, now)
	}
}

func (s *Scheduler) execute(sc ScheduleEntry) {
	slog.Info("scheduler: firing", "id", sc.ID, "chat_id", sc.ChatID, "type", sc.Type, "mode", sc.Mode, "label", sc.Label)

	msg := sc.Message

	if sc.Type == "heartbeat" {
		// Heartbeat always routes through Claude with session context.
		// Frame the message so Claude knows this is a periodic check-in.
		msg = "[Heartbeat] " + msg
		if s.onPrompt != nil {
			s.onPrompt(sc.ChatID, msg)
		}
		return
	}

	if sc.Label != "" && sc.Mode == "notify" {
		msg = "⏰ " + sc.Label + "\n\n" + msg
	}

	switch sc.Mode {
	case "prompt":
		if s.onPrompt != nil {
			s.onPrompt(sc.ChatID, msg)
		}
	default: // "notify"
		if s.onNotify != nil {
			s.onNotify(sc.ChatID, msg)
		}
	}
}

func (s *Scheduler) advance(sc ScheduleEntry, now time.Time) {
	if sc.Type == "once" {
		if err := s.store.DisableSchedule(sc.ID); err != nil {
			slog.Error("scheduler: failed to disable one-shot", "id", sc.ID, "error", err)
		}
		if err := s.store.UpdateScheduleNextRun(sc.ID, now, now); err != nil {
			slog.Error("scheduler: failed to update last_run", "id", sc.ID, "error", err)
		}
		return
	}

	if sc.Type == "heartbeat" {
		// Heartbeat uses interval-based advancement: schedule field is a Go duration string.
		interval, err := time.ParseDuration(sc.Schedule)
		if err != nil {
			slog.Error("scheduler: invalid heartbeat interval, disabling", "id", sc.ID, "interval", sc.Schedule, "error", err)
			s.store.DisableSchedule(sc.ID)
			return
		}
		nextRun := now.Add(interval)
		if err := s.store.UpdateScheduleNextRun(sc.ID, nextRun, now); err != nil {
			slog.Error("scheduler: failed to update next_run", "id", sc.ID, "error", err)
		}
		return
	}

	// Compute next run for cron schedules
	loc := s.loadLocation(sc.Timezone)
	cron, err := ParseCron(sc.Schedule)
	if err != nil {
		slog.Error("scheduler: invalid cron, disabling", "id", sc.ID, "expr", sc.Schedule, "error", err)
		s.store.DisableSchedule(sc.ID)
		return
	}

	nextRun := cron.Next(now.In(loc)).UTC()
	if nextRun.IsZero() {
		slog.Warn("scheduler: no next run found, disabling", "id", sc.ID)
		s.store.DisableSchedule(sc.ID)
		return
	}

	if err := s.store.UpdateScheduleNextRun(sc.ID, nextRun, now); err != nil {
		slog.Error("scheduler: failed to update next_run", "id", sc.ID, "error", err)
	}
}

func (s *Scheduler) loadLocation(tz string) *time.Location {
	if tz == "" {
		tz = s.defaultTZ
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		slog.Warn("scheduler: invalid timezone, falling back to UTC", "tz", tz, "error", err)
		return time.UTC
	}
	return loc
}
