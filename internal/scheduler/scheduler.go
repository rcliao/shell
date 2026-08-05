package scheduler

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rcliao/shell/internal/beat"
)

// ScheduleStore is the interface the scheduler needs from the store.
type ScheduleStore interface {
	GetDueSchedules(now time.Time) ([]ScheduleEntry, error)
	UpdateScheduleNextRun(id int64, nextRun time.Time, lastRun time.Time) error
	// SetExpectedNextAt records the EXACT next occurrence, before jitter.
	SetExpectedNextAt(id int64, expected time.Time) error
	DisableSchedule(id int64) error
	// PauseSchedule disables a schedule and records a machine-readable cause.
	// Used for unrecoverable config errors instead of retrying forever.
	PauseSchedule(id int64, reason string) error
	// BumpHeartbeatCount atomically increments and returns a schedule's persisted
	// heartbeat count, so deep-reflection cadence survives restarts.
	BumpHeartbeatCount(id int64) (int, error)
	// RecordJobRun appends a complete fire attempt (used for outcomes decided
	// before any work starts, e.g. an overlap skip).
	RecordJobRun(run JobRun) error
	// StartJobRun opens a run row when work begins; FinishJobRun closes it.
	// The split is what makes queue delay distinguishable from execution time.
	StartJobRun(run JobRun) (int64, error)
	// FinishJobRun closes it. scheduleID/firedAt are passed back so the store
	// never has to read the row inside its write transaction — that read-then-
	// upgrade is what returned SQLITE_BUSY and lost a run's terminal outcome.
	FinishJobRun(runID, scheduleID int64, firedAt time.Time, outcome, errMsg string) error
}

// JobRun is one recorded fire attempt (mirrors store.JobRun).
type JobRun struct {
	ScheduleID     int64
	TriggerContext string
	ScheduledAt    time.Time // zero = unknown
	FiredAt        time.Time
	Attempt        int
	Outcome        string
	ErrorMessage   string
}

// Job run outcomes and trigger contexts (mirror the store constants).
const (
	TriggerSchedule = "schedule"

	OutcomeFiredOK           = "fired_ok"
	OutcomeSpawnFailed       = "spawn_failed"
	OutcomeTurnFailed        = "turn_failed"
	OutcomeSkippedQuietHours = "skipped_quiet_hours"
	OutcomeSkippedOverlap    = "skipped_overlap"
)

// Auto-pause reasons (mirror the store constants).
const (
	PauseInvalidCron     = "invalid_cron"
	PauseInvalidInterval = "invalid_interval"
	PauseNoNextRun       = "no_next_run"
	PauseMissingChat     = "missing_chat"
)

// ScheduleEntry mirrors store.Schedule but avoids a circular import.
type ScheduleEntry struct {
	ID        int64
	ChatID    int64
	Label     string
	Message   string
	Schedule  string // cron expr or ISO8601
	Timezone  string
	Type      string // "cron" or "once"
	Mode      string // "notify" or "prompt"
	NextRunAt time.Time
	// Overlap is the stored overlap policy; empty means the type default.
	Overlap string
}

// NotifyFunc sends a plain text message to a chat.
type NotifyFunc func(chatID int64, msg string)

// PromptFunc routes a message through Claude as if the user sent it. The error
// is what makes a failed spawn/turn visible in the job_runs ledger instead of
// vanishing into the daemon's logs. The context carries the per-job timeout and
// daemon shutdown — an implementation that ignores it cannot be cancelled.
type PromptFunc func(ctx context.Context, chatID int64, msg string) error

// HeartbeatPromptFunc routes a heartbeat through Claude and returns the response.
// The scheduler uses the response to decide whether to send it (noop suppression).
type HeartbeatPromptFunc func(ctx context.Context, chatID int64, msg string) (string, error)

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

// defaultJobTimeout bounds a single fire attempt. Observed turns run to ~8.5
// minutes, so this is generous — its job is to stop a wedged turn from holding
// a chat's queue forever, not to cut normal work short.
const defaultJobTimeout = 20 * time.Minute

// Scheduler runs a 1-minute tick loop to fire due schedules.
type Scheduler struct {
	store         ScheduleStore
	onNotify      NotifyFunc
	onPrompt      PromptFunc
	onHeartbeat   HeartbeatPromptFunc
	onTaskPoll    TaskPollFunc    // polls for pending task events
	onTaskProcess TaskProcessFunc // processes a task event
	defaultTZ     string
	quietHours    QuietHours
	// hbMu guards the heartbeat maps. Jobs run on per-chat worker goroutines,
	// so these are touched concurrently across chats — they were plain maps
	// while everything ran on the single tick goroutine.
	hbMu                sync.Mutex
	heartbeatCounts     map[int64]int  // chat_id → number of heartbeats fired (for check-in cadence)
	heartbeatIdle       map[int64]bool // chat_id → true if last heartbeat was noop
	idleInterval        time.Duration  // interval after noop heartbeat (0 = use schedule interval)
	deepReflectInterval int            // every Nth heartbeat escalates to deep reflection (0 = use default)
	jitterFrac          JitterFracFunc // firing jitter source; nil disables jitter
	dispatch            *dispatcher
	jobTimeout          time.Duration
	retry               RetryPolicy
	// queue, when set, replaces the in-memory dispatcher with durable
	// execution: fires become rows that survive the process. queueOwner is the
	// daemon's boot id, which is how a reclaim tells "my lease" from "a lease
	// held by a process that no longer exists". See queue.go.
	queue      TaskQueue
	queueOwner string
	queueWG    sync.WaitGroup
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
		jitterFrac:      NewSeededJitter(time.Now().UnixNano()),
		dispatch:        newDispatcher(16),
		jobTimeout:      defaultJobTimeout,
		retry:           DefaultRetryPolicy(),
	}
}

// SetJobTimeout bounds a single fire attempt. Zero restores the default.
func (s *Scheduler) SetJobTimeout(d time.Duration) {
	if d <= 0 {
		d = defaultJobTimeout
	}
	s.jobTimeout = d
}

// SetRetryPolicy replaces the retry policy for failed fires.
func (s *Scheduler) SetRetryPolicy(p RetryPolicy) {
	if p.MaxAttempts < 1 {
		p.MaxAttempts = 1
	}
	s.retry = p
}

// SetJitterSource replaces the firing-jitter fraction source. Pass nil to
// disable jitter entirely (fires land exactly on the occurrence). Tests inject
// a fixed source so jittered next-run times are reproducible.
func (s *Scheduler) SetJitterSource(fn JitterFracFunc) {
	s.jitterFrac = fn
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

	if s.queue != nil {
		slog.Info("scheduler: durable fire queue enabled", "owner", s.queueOwner, "workers", queueWorkers)
		s.runQueueWorkers(ctx, queueWorkers)
	}

	// Run immediately on start, then on each tick
	s.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler stopped")
			s.dispatch.wait()
			s.queueWG.Wait()
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick finds due schedules and hands each to its chat's worker. It must never
// block on the work itself: the tick goroutine is shared by every schedule, and
// a fire that runs inline makes every other schedule wait behind it.
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now().UTC()
	schedules, err := s.store.GetDueSchedules(now)
	if err != nil {
		slog.Error("scheduler: failed to get due schedules", "error", err)
		return
	}

	if s.queue != nil {
		// Reclaim before enqueueing. A fire abandoned by the previous process
		// must be back in the queue before this tick's overlap check runs, or
		// the check would not see it and would happily queue a duplicate.
		s.sweepQueue()
	}

	for _, sc := range schedules {
		policy := ParseOverlapPolicy(sc.Overlap, sc.Type)
		submitted := false
		if s.queue != nil {
			submitted = s.submitToQueue(sc, policy, sc.NextRunAt)
		} else {
			submitted = s.dispatch.submit(ctx, sc, policy, s.runJob)
		}
		if !submitted {
			slog.Info("scheduler: overlap policy declined fire",
				"id", sc.ID, "label", sc.Label, "policy", policy)
			s.recordComplete(sc, OutcomeSkippedOverlap, string(policy))
		}
		// Advance BEFORE the job runs, not after. Work now happens off this
		// goroutine, so a schedule whose next_run_at is still in the past would
		// be re-selected on every tick while its job is in flight. Advancing
		// here also fixes the semantics: a crash mid-turn leaves a visible
		// 'running' row (reaped to 'interrupted') instead of silently re-firing
		// on restart and delivering the same reminder twice.
		s.advance(sc, now)
	}

	// Poll shared task store for pending task events.
	s.pollTasks()
}

// runJob executes one fire with its retry policy, recording every attempt as
// its own ledger row. Runs on a chat's worker goroutine.
func (s *Scheduler) runJob(ctx context.Context, sc ScheduleEntry) {
	for attempt := 1; attempt <= s.retry.MaxAttempts; attempt++ {
		firedAt := time.Now().UTC()
		runID, err := s.store.StartJobRun(JobRun{
			ScheduleID:     sc.ID,
			TriggerContext: TriggerSchedule,
			ScheduledAt:    sc.NextRunAt,
			FiredAt:        firedAt,
			Attempt:        attempt,
		})
		if err != nil {
			slog.Warn("scheduler: failed to open job run", "id", sc.ID, "error", err)
		}

		jobCtx, cancel := context.WithTimeout(ctx, s.jobTimeout)
		outcome, errMsg, runErr := s.execute(jobCtx, sc, runID)
		cancel()

		if runID != 0 {
			if err := s.store.FinishJobRun(runID, sc.ID, firedAt, outcome, errMsg); err != nil {
				slog.Warn("scheduler: failed to close job run", "id", sc.ID, "run_id", runID, "error", err)
			}
		}

		if runErr == nil || !IsRetryable(runErr) {
			return
		}
		if attempt == s.retry.MaxAttempts {
			slog.Error("scheduler: retries exhausted", "id", sc.ID, "label", sc.Label,
				"attempts", attempt, "error", runErr)
			return
		}

		wait := s.retry.Backoff(attempt + 1)
		slog.Warn("scheduler: transient failure, retrying",
			"id", sc.ID, "label", sc.Label, "attempt", attempt, "wait", wait, "error", runErr)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
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

// recordComplete writes a single already-finished ledger row. Used for outcomes
// decided before any work starts (an overlap skip), where there is no interval
// to measure and StartJobRun/FinishJobRun would be two writes for one instant.
func (s *Scheduler) recordComplete(sc ScheduleEntry, outcome, errMsg string) {
	if s.store == nil {
		return
	}
	now := time.Now().UTC()
	if err := s.store.RecordJobRun(JobRun{
		ScheduleID:     sc.ID,
		TriggerContext: TriggerSchedule,
		ScheduledAt:    sc.NextRunAt,
		FiredAt:        now,
		Attempt:        1,
		Outcome:        outcome,
		ErrorMessage:   errMsg,
	}); err != nil {
		slog.Warn("scheduler: failed to record job run", "id", sc.ID, "outcome", outcome, "error", err)
	}
}

// heartbeatFallbackCount bumps the in-memory heartbeat counter for a chat.
// Only used when the persistent bump fails.
func (s *Scheduler) heartbeatFallbackCount(chatID int64) int {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	s.heartbeatCounts[chatID]++
	return s.heartbeatCounts[chatID]
}

func (s *Scheduler) setHeartbeatIdle(chatID int64, idle bool) {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	s.heartbeatIdle[chatID] = idle
}

func (s *Scheduler) isHeartbeatIdle(chatID int64) bool {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	return s.heartbeatIdle[chatID]
}

// execute runs one fire attempt and reports its outcome. The returned error is
// the raw failure, kept separate from the message so the caller can classify it
// for retry; a nil error with a non-OK outcome means "failed, do not retry".
func (s *Scheduler) execute(ctx context.Context, sc ScheduleEntry, runID int64) (outcome, errMsg string, err error) {
	slog.Info("scheduler: firing", "id", sc.ID, "chat_id", sc.ChatID, "type", sc.Type, "mode", sc.Mode, "label", sc.Label)

	msg := sc.Message

	// A schedule with no target chat can never deliver. Auto-pause with a
	// machine-readable reason instead of retrying every minute forever.
	//
	// Heartbeats are exempt: chat_id 0 is their NORMAL state — the system
	// chat — and the bridge decides where (or whether) their output lands.
	// Treating 0 as missing paused a live heartbeat on the first fire after
	// this check shipped (7/29).
	if sc.ChatID == 0 && sc.Type != "heartbeat" {
		slog.Error("scheduler: schedule has no chat_id, auto-pausing", "id", sc.ID, "label", sc.Label)
		s.pause(sc.ID, PauseMissingChat)
		return OutcomeSpawnFailed, "missing chat_id", nil
	}

	if sc.Type == "heartbeat" {
		// Suppress heartbeats during quiet hours.
		if s.isQuietHours() {
			slog.Info("scheduler: skipping heartbeat during quiet hours", "id", sc.ID, "chat_id", sc.ChatID)
			return OutcomeSkippedQuietHours, "", nil
		}

		// Track heartbeat count for check-in + deep-reflection cadence. Persist
		// it so the count (and thus the "every Nth heartbeat is deep" cadence)
		// survives daemon restarts — an in-memory counter reset on every re-exec
		// and starved deep reflection entirely. Fall back to in-memory if the
		// persistent bump fails, so heartbeats keep working.
		count, bumpErr := s.store.BumpHeartbeatCount(sc.ID)
		if bumpErr != nil {
			count = s.heartbeatFallbackCount(sc.ChatID)
			slog.Warn("scheduler: persistent heartbeat count failed, using in-memory", "id", sc.ID, "chat_id", sc.ChatID, "error", bumpErr)
		}

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

		// Attach fire metadata for the reflection journal. Done here rather
		// than in runJob because the counter is only known after the bump —
		// and the counter is what places a journal row in the deep-reflection
		// sequence instead of leaving it to timestamp correlation.
		ctx = beat.With(ctx, beat.Meta{RunID: runID, Count: count, Deep: isDeep})

		// Every ~4 heartbeats, add a check-in hint.
		if count%4 == 0 {
			msg += "\n\n[Check-in: It's been a while — feel free to send a brief friendly check-in message along with your regular update.]"
		}

		if s.onHeartbeat != nil {
			hbStart := time.Now()
			resp, hbErr := s.onHeartbeat(ctx, sc.ChatID, msg)
			hbElapsed := time.Since(hbStart)
			if hbErr != nil {
				slog.Error("scheduler: heartbeat prompt failed", "chat_id", sc.ChatID, "elapsed", hbElapsed, "error", hbErr)
				return OutcomeTurnFailed, hbErr.Error(), hbErr
			}
			noop := isHeartbeatNoop(resp)
			s.setHeartbeatIdle(sc.ChatID, noop)
			slog.Info("scheduler: heartbeat completed",
				"chat_id", sc.ChatID,
				"noop", noop,
				"resp_len", len(resp),
				"elapsed", hbElapsed,
				"count", count,
			)
			if noop {
				return OutcomeFiredOK, "", nil
			}
			if s.onNotify != nil {
				s.onNotify(sc.ChatID, resp)
			}
			return OutcomeFiredOK, "", nil
		}
		if s.onPrompt != nil {
			// Fallback to onPrompt if onHeartbeat not set.
			if err := s.onPrompt(ctx, sc.ChatID, msg); err != nil {
				return OutcomeTurnFailed, err.Error(), err
			}
			return OutcomeFiredOK, "", nil
		}
		return OutcomeSpawnFailed, "no heartbeat or prompt handler wired", nil
	}

	if sc.Label != "" && sc.Mode == "notify" {
		msg = "\u23f0 " + sc.Label + "\n\n" + msg
	}

	switch sc.Mode {
	case "prompt":
		if s.onPrompt == nil {
			slog.Error("scheduler: prompt-mode schedule fired with no prompt handler", "id", sc.ID)
			return OutcomeSpawnFailed, "no prompt handler wired", nil
		}
		if err := s.onPrompt(ctx, sc.ChatID, msg); err != nil {
			slog.Error("scheduler: scheduled prompt failed", "id", sc.ID, "chat_id", sc.ChatID, "error", err)
			return OutcomeTurnFailed, err.Error(), err
		}
		return OutcomeFiredOK, "", nil
	default: // "notify"
		if s.onNotify == nil {
			slog.Error("scheduler: notify-mode schedule fired with no notify handler", "id", sc.ID)
			return OutcomeSpawnFailed, "no notify handler wired", nil
		}
		s.onNotify(sc.ChatID, msg)
		return OutcomeFiredOK, "", nil
	}
}

// pause auto-disables a schedule after an unrecoverable config error and stores
// a machine-readable reason. Falls back to a plain disable if the store predates
// PauseSchedule.
func (s *Scheduler) pause(id int64, reason string) {
	if err := s.store.PauseSchedule(id, reason); err != nil {
		slog.Error("scheduler: failed to auto-pause schedule", "id", id, "reason", reason, "error", err)
		s.store.DisableSchedule(id)
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
			slog.Error("scheduler: invalid heartbeat interval, auto-pausing", "id", sc.ID, "interval", sc.Schedule, "error", err)
			s.pause(sc.ID, PauseInvalidInterval)
			return
		}
		// Use idle interval if last heartbeat was noop and idle interval is configured.
		if s.isHeartbeatIdle(sc.ChatID) && s.idleInterval > 0 && s.idleInterval > interval {
			slog.Info("scheduler: using idle interval for next heartbeat",
				"id", sc.ID, "chat_id", sc.ChatID,
				"normal", interval, "idle", s.idleInterval)
			interval = s.idleInterval
		}
		exact := now.Add(interval)

		// If next run falls in quiet hours, push it to the end of quiet hours.
		loc := s.loadLocation(s.defaultTZ)
		nextLocal := exact.In(loc)
		if s.isQuietAt(nextLocal) {
			nextLocal = s.nextWakeTime(nextLocal)
			exact = nextLocal.UTC()
			slog.Info("scheduler: pushed heartbeat past quiet hours", "id", sc.ID, "next_run", exact)
		}

		// Jitter the firing moment so two agents' heartbeats don't collide.
		// The following occurrence bounds it, so jitter can never swallow a beat.
		nextRun := applyJitter(exact, interval, exact.Add(interval), s.jitterFor(sc.ID))
		s.persistNextRun(sc.ID, nextRun, exact, now)
		return
	}

	// Compute next run for cron schedules
	loc := s.loadLocation(sc.Timezone)
	cron, err := ParseCron(sc.Schedule)
	if err != nil {
		slog.Error("scheduler: invalid cron, auto-pausing", "id", sc.ID, "expr", sc.Schedule, "error", err)
		s.pause(sc.ID, PauseInvalidCron)
		return
	}

	// The EXACT occurrence stays exact — jitter is applied to the firing moment
	// only, and the next computation always starts from real time, so drift
	// cannot accumulate.
	exactLocal := cron.Next(now.In(loc))
	if exactLocal.IsZero() {
		slog.Warn("scheduler: no next run found, auto-pausing", "id", sc.ID)
		s.pause(sc.ID, PauseNoNextRun)
		return
	}
	exact := exactLocal.UTC()
	following := cron.Next(exactLocal)
	interval := time.Duration(0)
	if !following.IsZero() {
		interval = following.Sub(exactLocal)
	}
	nextRun := applyJitter(exact, interval, following.UTC(), s.jitterFor(sc.ID))
	s.persistNextRun(sc.ID, nextRun, exact, now)
}

// persistNextRun writes the jittered firing moment (next_run_at) alongside the
// exact occurrence (expected_next_at). The dead-man's-switch and the preview
// read the exact column; only the tick loop reads the jittered one.
func (s *Scheduler) persistNextRun(id int64, nextRun, exact, lastRun time.Time) {
	if err := s.store.UpdateScheduleNextRun(id, nextRun, lastRun); err != nil {
		slog.Error("scheduler: failed to update next_run", "id", id, "error", err)
		return
	}
	if err := s.store.SetExpectedNextAt(id, exact); err != nil {
		slog.Warn("scheduler: failed to record expected_next_at", "id", id, "error", err)
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
