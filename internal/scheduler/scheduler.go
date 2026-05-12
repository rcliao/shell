package scheduler

import (
	"context"
	"log/slog"
	"strings"
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

// HeartbeatPromptFunc routes a heartbeat through Claude and returns the response.
// The scheduler uses the response to decide whether to send it (noop suppression).
type HeartbeatPromptFunc func(chatID int64, msg string) (string, error)

// TaskPollFunc polls the shared task store for events targeting this agent.
// Returns event payloads (JSON strings) for task.created events that need processing.
// Called on every scheduler tick.
type TaskPollFunc func() []string

// TaskProcessFunc fires a Claude session to process a task event.
// Receives the event payload JSON.
type TaskProcessFunc func(payload string)

// QuietHours defines the window during which heartbeats are suppressed.
type QuietHours struct {
	Start int // hour (0-23) when quiet hours begin
	End   int // hour (0-23) when quiet hours end
}

// defaultDeepReflectInterval is the default number of heartbeats between deep reflection runs.
const defaultDeepReflectInterval = 6

// Scheduler runs a 1-minute tick loop to fire due schedules.
type Scheduler struct {
	store                ScheduleStore
	onNotify             NotifyFunc
	onPrompt             PromptFunc
	onHeartbeat          HeartbeatPromptFunc
	onTaskPoll           TaskPollFunc    // polls for pending task events
	onTaskProcess        TaskProcessFunc // processes a task event
	defaultTZ            string
	quietHours           QuietHours
	heartbeatCounts      map[int64]int  // chat_id → number of heartbeats fired (for check-in cadence)
	heartbeatIdle        map[int64]bool // chat_id → true if last heartbeat was noop
	idleInterval         time.Duration  // interval after noop heartbeat (0 = use schedule interval)
	deepReflectInterval  int            // every Nth heartbeat escalates to deep reflection (0 = use default)
}

// New creates a new Scheduler.
func New(store ScheduleStore, onNotify NotifyFunc, onPrompt PromptFunc, defaultTZ string) *Scheduler {
	if defaultTZ == "" {
		defaultTZ = "UTC"
	}
	return &Scheduler{
		store:           store,
		onNotify:        onNotify,
		onPrompt:        onPrompt,
		defaultTZ:       defaultTZ,
		quietHours:      QuietHours{Start: 22, End: 7},
		heartbeatCounts: make(map[int64]int),
		heartbeatIdle:   make(map[int64]bool),
	}
}

// SetQuietHours configures the quiet hours window for heartbeats.
func (s *Scheduler) SetQuietHours(start, end int) {
	s.quietHours = QuietHours{Start: start, End: end}
}

// SetHeartbeatPrompt sets the heartbeat-specific prompt function that returns the response.
func (s *Scheduler) SetHeartbeatPrompt(fn HeartbeatPromptFunc) {
	s.onHeartbeat = fn
}

// SetIdleInterval configures the interval used after a noop heartbeat.
// When set, heartbeats that produce no output will schedule the next run
// at this longer interval instead of the default.
func (s *Scheduler) SetIdleInterval(d time.Duration) {
	s.idleInterval = d
}

// SetDeepReflectInterval sets how often (every N heartbeats) the deep
// reflection model is used instead of the regular heartbeat model.
func (s *Scheduler) SetDeepReflectInterval(n int) {
	s.deepReflectInterval = n
}

// SetTaskPoll configures the task polling and processing callbacks.
// poll returns event payloads for pending tasks; process fires a Claude session per task.
func (s *Scheduler) SetTaskPoll(poll TaskPollFunc, process TaskProcessFunc) {
	s.onTaskPoll = poll
	s.onTaskProcess = process
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

	// Poll shared task store for pending task events.
	s.pollTasks()
}

// pollTasks checks for pending task events and processes each in a goroutine.
func (s *Scheduler) pollTasks() {
	if s.onTaskPoll == nil || s.onTaskProcess == nil {
		return
	}

	payloads := s.onTaskPoll()
	for _, p := range payloads {
		payload := p // capture for goroutine
		go func() {
			slog.Info("scheduler: processing task event", "payload_len", len(payload))
			s.onTaskProcess(payload)
		}()
	}
}

// isQuietHours checks whether the current time falls within quiet hours
// in the configured timezone.
func (s *Scheduler) isQuietHours() bool {
	loc := s.loadLocation(s.defaultTZ)
	hour := time.Now().In(loc).Hour()

	start := s.quietHours.Start
	end := s.quietHours.End

	if start == end {
		return false // no quiet hours if start == end
	}

	if start > end {
		// Wraps midnight: e.g. 22:00 - 07:00
		return hour >= start || hour < end
	}
	// Same day: e.g. 01:00 - 06:00
	return hour >= start && hour < end
}

// isHeartbeatNoop returns true if the heartbeat response indicates nothing
// noteworthy happened and should not be sent to the user.
func isHeartbeatNoop(response string) bool {
	if strings.TrimSpace(response) == "" {
		return true
	}

	lower := strings.ToLower(response)

	// Common noop phrases from Claude when there's nothing to report
	noopPhrases := []string{
		"nothing to report",
		"no new activity",
		"nothing noteworthy",
		"no updates",
		"all quiet",
		"nothing requires attention",
		"no action needed",
		"nothing needs attention",
		"no items require",
		"everything looks good",
		"no pending tasks",
		"[noop]",
	}

	for _, phrase := range noopPhrases {
		if strings.Contains(lower, phrase) {
			// Only treat as noop if the response is short (< 200 chars).
			// Longer responses with these phrases likely have additional context.
			if len(response) < 200 {
				return true
			}
		}
	}

	return false
}

func (s *Scheduler) execute(sc ScheduleEntry) {
	slog.Info("scheduler: firing", "id", sc.ID, "chat_id", sc.ChatID, "type", sc.Type, "mode", sc.Mode, "label", sc.Label)

	msg := sc.Message

	if sc.Type == "heartbeat" {
		// Suppress heartbeats during quiet hours.
		if s.isQuietHours() {
			slog.Info("scheduler: skipping heartbeat during quiet hours", "id", sc.ID, "chat_id", sc.ChatID)
			return
		}

		// Track heartbeat count for check-in cadence.
		s.heartbeatCounts[sc.ChatID]++
		count := s.heartbeatCounts[sc.ChatID]

		// Determine deep reflection interval.
		deepInterval := s.deepReflectInterval
		if deepInterval <= 0 {
			deepInterval = defaultDeepReflectInterval
		}

		// Every Nth heartbeat, escalate to deep reflection for behavioral learning.
		isDeep := count%deepInterval == 0
		if isDeep {
			msg = "[Heartbeat:deep] " + msg
			slog.Info("scheduler: deep reflection heartbeat", "chat_id", sc.ChatID, "count", count, "interval", deepInterval)
		} else {
			msg = "[Heartbeat] " + msg
		}

		// Every ~4 heartbeats, add a check-in hint.
		if count%4 == 0 {
			msg += "\n\n[Check-in: It's been a while — feel free to send a brief friendly check-in message along with your regular update.]"
		}

		if s.onHeartbeat != nil {
			hbStart := time.Now()
			resp, err := s.onHeartbeat(sc.ChatID, msg)
			hbElapsed := time.Since(hbStart)
			if err != nil {
				slog.Error("scheduler: heartbeat prompt failed", "chat_id", sc.ChatID, "elapsed", hbElapsed, "error", err)
				return
			}
			noop := isHeartbeatNoop(resp)
			s.heartbeatIdle[sc.ChatID] = noop
			slog.Info("scheduler: heartbeat completed",
				"chat_id", sc.ChatID,
				"noop", noop,
				"resp_len", len(resp),
				"elapsed", hbElapsed,
				"count", count,
			)
			if noop {
				return
			}
			if s.onNotify != nil {
				s.onNotify(sc.ChatID, resp)
			}
		} else if s.onPrompt != nil {
			// Fallback to onPrompt if onHeartbeat not set.
			s.onPrompt(sc.ChatID, msg)
		}
		return
	}

	if sc.Label != "" && sc.Mode == "notify" {
		msg = "\u23f0 " + sc.Label + "\n\n" + msg
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
		// Use idle interval if last heartbeat was noop and idle interval is configured.
		if s.heartbeatIdle[sc.ChatID] && s.idleInterval > 0 && s.idleInterval > interval {
			slog.Info("scheduler: using idle interval for next heartbeat",
				"id", sc.ID, "chat_id", sc.ChatID,
				"normal", interval, "idle", s.idleInterval)
			interval = s.idleInterval
		}
		nextRun := now.Add(interval)

		// If next run falls in quiet hours, push it to the end of quiet hours.
		loc := s.loadLocation(s.defaultTZ)
		nextLocal := nextRun.In(loc)
		if s.isQuietAt(nextLocal) {
			nextLocal = s.nextWakeTime(nextLocal)
			nextRun = nextLocal.UTC()
			slog.Info("scheduler: pushed heartbeat past quiet hours", "id", sc.ID, "next_run", nextRun)
		}

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

// isQuietAt checks if a local time falls within quiet hours.
func (s *Scheduler) isQuietAt(t time.Time) bool {
	hour := t.Hour()
	start := s.quietHours.Start
	end := s.quietHours.End

	if start == end {
		return false
	}
	if start > end {
		return hour >= start || hour < end
	}
	return hour >= start && hour < end
}

// nextWakeTime returns the next time after quiet hours end for a given local time.
func (s *Scheduler) nextWakeTime(t time.Time) time.Time {
	end := s.quietHours.End
	// Move to the same day at the end hour
	wake := time.Date(t.Year(), t.Month(), t.Day(), end, 0, 0, 0, t.Location())
	if !wake.After(t) {
		// If wake is before or equal to t, it means we need next day's wake time
		wake = wake.Add(24 * time.Hour)
	}
	return wake
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
