package scheduler

import (
	"strconv"
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
			Overlap:   sc.OverlapPolicy,
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
	return a.s.RecordJobRun(toStoreRun(run))
}

func (a *StoreAdapter) StartJobRun(run JobRun) (int64, error) {
	return a.s.StartJobRun(toStoreRun(run))
}

func (a *StoreAdapter) FinishJobRun(runID, scheduleID int64, firedAt time.Time, outcome, errMsg string) error {
	return a.s.FinishJobRunAt(runID, scheduleID, firedAt, outcome, errMsg)
}

func toStoreRun(run JobRun) store.JobRun {
	rec := store.JobRun{
		ScheduleID:     run.ScheduleID,
		TriggerContext: run.TriggerContext,
		FiredAt:        run.FiredAt,
		Attempt:        run.Attempt,
		Outcome:        run.Outcome,
		ErrorMessage:   run.ErrorMessage,
	}
	if !run.ScheduledAt.IsZero() {
		t := run.ScheduledAt
		rec.ScheduledAt = &t
	}
	return rec
}

// --- TaskQueue: durable execution for scheduled fires. See queue.go. ---

// EnqueueFire registers one occurrence of a schedule as a durable task.
//
// The idempotency key is (schedule id, occurrence), NOT a content hash: two
// legitimately identical beats an hour apart must both run, and the same beat
// offered twice by a duplicate tick or a restart must run once. The occurrence
// time is exactly the thing that distinguishes those cases.
func (a *StoreAdapter) EnqueueFire(entry ScheduleEntry, occurrence, expiresAt time.Time, maxAttempts int) (bool, error) {
	payload, err := encodeFirePayload(entry)
	if err != nil {
		return false, err
	}
	exp := expiresAt
	_, created, err := a.s.EnqueueTask(store.Task{
		Kind:   TaskKindScheduleFire,
		Source: store.TaskSourceScheduler,
		IdempotencyKey: store.DeriveIdempotencyKey(
			TaskKindScheduleFire,
			strconv.FormatInt(entry.ID, 10),
			occurrence.UTC().Format(time.RFC3339Nano),
		),
		PartitionKey: FirePartitionKey(entry.ChatID),
		Payload:      payload,
		MaxAttempts:  maxAttempts,
		ExpiresAt:    &exp,
	})
	return created, err
}

func (a *StoreAdapter) LeaseFire(owner string, leaseFor time.Duration) (*QueuedFire, error) {
	t, err := a.s.LeaseTask(owner, leaseFor)
	if err != nil || t == nil {
		return nil, err
	}
	if t.Kind != TaskKindScheduleFire {
		// Another kind reached a scheduler worker. Nothing else enqueues yet, so
		// this is unreachable today; fail the task rather than silently drop it
		// so the first non-fire kind shows up loudly instead of stalling.
		_, ferr := a.s.FailTask(t.ID, "no handler: scheduler workers only run "+TaskKindScheduleFire)
		return nil, ferr
	}
	entry, err := decodeFirePayload(t.Payload)
	if err != nil {
		_, ferr := a.s.FailTask(t.ID, "undecodable payload: "+err.Error())
		if ferr != nil {
			return nil, ferr
		}
		return nil, err
	}
	return &QueuedFire{TaskID: t.ID, Entry: entry, Attempt: t.Attempts}, nil
}

func (a *StoreAdapter) CompleteFire(taskID int64, result string) error {
	return a.s.CompleteTask(taskID, result)
}

func (a *StoreAdapter) FailFire(taskID int64, errMsg string) (bool, error) {
	return a.s.FailTask(taskID, errMsg)
}

func (a *StoreAdapter) ReclaimFires(currentOwner string) (int, error) {
	return a.s.ReclaimTasks(currentOwner)
}

func (a *StoreAdapter) ExpireFires() (int, error) {
	return a.s.ExpireTasks()
}

func (a *StoreAdapter) CountActiveFires(scheduleID int64) (int, error) {
	return a.s.CountActiveByPayloadInt(TaskKindScheduleFire, payloadSchedulePath, scheduleID)
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
