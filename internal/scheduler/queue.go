package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Durable execution for scheduled fires.
//
// The scheduler still decides WHEN; the queue owns RUNNING. The difference that
// matters is what happens when the daemon dies mid-fire. With the in-memory
// dispatcher the work simply vanished: the ledger recorded `interrupted` and
// nothing ever picked it up again. A deep reflection beat was lost that way on
// 2026-08-01 after 340 seconds of work — the most expensive single unit of work
// in the system, discarded with no recovery path, because none existed.
//
// A queued fire survives the process. On restart the lease is reclaimed (its
// owner is a boot id that no longer exists, so it cannot still be running) and
// the task is leased again by the new process.
//
// Replay has to know when to stop, which is what ExpiresAt is for. A chat turn
// reclaimed after a crash is still worth running because the human is still
// waiting. A heartbeat reclaimed three hours late is not — it would report on a
// world that has moved on. So a fire carries an expiry derived from its own
// cadence, and past it the task is dropped with a recorded outcome.

// TaskKindScheduleFire is the queue kind for a scheduled fire.
const TaskKindScheduleFire = "schedule.fire"

// payloadSchedulePath is the JSON path to a fire's schedule id, used to ask the
// queue the overlap question.
const payloadSchedulePath = "$.schedule_id"

// fireLeaseGrace pads the lease beyond the job timeout. The lease must outlive
// the work it protects — a lease that expires under a running turn invites a
// second worker to start the same fire, which is precisely the duplicate the
// queue exists to prevent.
const fireLeaseGrace = 10 * time.Minute

// fireMaxAttempts allows exactly ONE replay. The first lease is the normal run;
// if the process dies the reclaimed task gets a second. Beyond that a fire that
// keeps killing its worker is a fire that should stop being retried, not one
// that should loop. In-turn transient failures are handled by the retry ladder
// in runJob, which is a different concern at a different layer.
const fireMaxAttempts = 2

// QueuedFire is a scheduled fire that has been handed to the queue.
type QueuedFire struct {
	TaskID  int64
	Entry   ScheduleEntry
	Attempt int
}

// LeasedTask is any leased unit of work, of any kind. The workers dispatch on
// Kind through the handler registry; only the schedule.fire handler knows how
// to read a fire payload.
type LeasedTask struct {
	ID      int64
	Kind    string
	Payload string
	Attempt int
}

// Handler runs one task and reports what it produced.
//
// Returning an error fails the attempt: the task returns to queued while
// attempts remain and becomes terminally failed once they are exhausted. The
// returned string is the task's result, which is what an assigner reads back.
type Handler func(ctx context.Context, t LeasedTask) (result string, err error)

// RegisterHandler binds a kind to its handler. A leased task whose kind has no
// handler fails immediately rather than being silently dropped or retried — an
// unregistered kind is a wiring bug, and retrying it would just hide it.
func (s *Scheduler) RegisterHandler(kind string, h Handler) {
	if s.handlers == nil {
		s.handlers = map[string]Handler{}
	}
	s.handlers[kind] = h
}

// TaskQueue is the durable side of firing. Implemented by StoreAdapter.
type TaskQueue interface {
	// EnqueueFire registers one occurrence. Idempotent on (schedule, occurrence):
	// enqueuing the same occurrence twice returns created=false.
	EnqueueFire(entry ScheduleEntry, occurrence time.Time, expiresAt time.Time, maxAttempts int) (created bool, err error)
	// LeaseNext claims the next ready task of ANY kind, or nil when none is
	// ready. Kind-specific decoding belongs to that kind's handler, not here:
	// the queue is generic infrastructure and scheduled fires are one producer.
	LeaseNext(owner string, leaseFor time.Duration) (*LeasedTask, error)
	CompleteFire(taskID int64, result string) error
	FailFire(taskID int64, errMsg string) (terminal bool, err error)
	// FailPermanent marks a task failed outright, ignoring remaining attempts,
	// for failures that cannot come out differently on a retry.
	FailPermanent(taskID int64, errMsg string) error
	// ReclaimFires returns leases held by a dead owner or past their expiry.
	ReclaimFires(currentOwner string) (int, error)
	// ExpireFires drops queued work that is no longer worth starting.
	ExpireFires() (int, error)
	// CountActiveFires counts queued+leased fires for one schedule — the
	// overlap question, asked of the table rather than an in-memory map.
	CountActiveFires(scheduleID int64) (int, error)
}

// firePayload is what a task carries. It is deliberately SELF-CONTAINED: a
// replayed task must know what to run without re-reading the schedule row,
// because by then the row has advanced to its next occurrence and may have been
// edited or disabled.
type firePayload struct {
	ScheduleID int64     `json:"schedule_id"`
	ChatID     int64     `json:"chat_id"`
	Label      string    `json:"label"`
	Message    string    `json:"message"`
	Schedule   string    `json:"schedule"`
	Timezone   string    `json:"timezone"`
	Type       string    `json:"type"`
	Mode       string    `json:"mode"`
	Overlap    string    `json:"overlap"`
	NextRunAt  time.Time `json:"next_run_at"`
}

func encodeFirePayload(e ScheduleEntry) (string, error) {
	b, err := json.Marshal(firePayload{
		ScheduleID: e.ID, ChatID: e.ChatID, Label: e.Label, Message: e.Message,
		Schedule: e.Schedule, Timezone: e.Timezone, Type: e.Type, Mode: e.Mode,
		Overlap: e.Overlap, NextRunAt: e.NextRunAt,
	})
	return string(b), err
}

func decodeFirePayload(s string) (ScheduleEntry, error) {
	var p firePayload
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return ScheduleEntry{}, err
	}
	return ScheduleEntry{
		ID: p.ScheduleID, ChatID: p.ChatID, Label: p.Label, Message: p.Message,
		Schedule: p.Schedule, Timezone: p.Timezone, Type: p.Type, Mode: p.Mode,
		Overlap: p.Overlap, NextRunAt: p.NextRunAt,
	}, nil
}

// PartitionKey names the serialization domain for work in one conversation.
//
// It is (chat, thread) because that is how SESSIONS are keyed: there is one
// Claude subprocess per pair, and the family group alone runs five of them, one
// per forum topic. The dispatcher's mailboxes and this key both exist for the
// same reason — two concurrent turns would contend for one subprocess.
//
// Keying on chat alone, which is what the dispatcher did and what this function
// did when first written, over-serializes: five topics that each own a separate
// subprocess would queue behind each other for no reason. Harmless for
// heartbeats, which run on the system chat, but wrong for anything per-topic —
// and message intake is entirely per-topic.
func PartitionKey(chatID, threadID int64) string {
	return fmt.Sprintf("chat:%d:%d", chatID, threadID)
}

// SetQueue enables durable execution. Without it the scheduler keeps using the
// in-memory dispatcher, which is still the path exercised by the unit tests
// that do not care about persistence.
func (s *Scheduler) SetQueue(q TaskQueue, owner string) {
	s.queue = q
	s.queueOwner = owner
	// Buffered by one: a wake is a hint that work exists, not a message that
	// must be delivered. If a wake is already pending the workers are about to
	// look anyway, so dropping the duplicate is correct.
	s.queueWake = make(chan struct{}, 1)
	s.RegisterHandler(TaskKindScheduleFire, s.handleScheduleFire)
}

// defaultFireTTL derives how long a fire stays worth running from its own
// cadence: a beat is worth replaying within its own period and not after.
// Falls back to a conservative hour when the interval is unknown.
func defaultFireTTL(interval time.Duration) time.Duration {
	if interval <= 0 {
		return time.Hour
	}
	// Clamp: a 1-minute schedule should still tolerate a short restart, and a
	// daily one should not stay replayable for a whole day.
	if interval < 15*time.Minute {
		return 15 * time.Minute
	}
	if interval > 6*time.Hour {
		return 6 * time.Hour
	}
	return interval
}

// scheduleInterval reports a schedule's cadence, or 0 when it has none.
// Heartbeats carry a Go duration; cron and one-shots do not, and fall back to
// the conservative default.
func (s *Scheduler) scheduleInterval(sc ScheduleEntry) time.Duration {
	if sc.Type != "heartbeat" {
		return 0
	}
	d, err := time.ParseDuration(sc.Schedule)
	if err != nil {
		return 0
	}
	return d
}

// submitToQueue enqueues one occurrence, applying the overlap policy first.
// Returns false when the policy declined, matching dispatcher.submit.
func (s *Scheduler) submitToQueue(sc ScheduleEntry, policy OverlapPolicy, occurrence time.Time) bool {
	if policy != OverlapAllow {
		active, err := s.queue.CountActiveFires(sc.ID)
		if err != nil {
			// Never let a bookkeeping read block a fire. Failing open risks a
			// duplicate beat, which is noop-suppressed and dedup'd downstream;
			// failing closed drops a reminder, which is not recoverable.
			slog.Warn("scheduler: overlap check failed, firing anyway", "id", sc.ID, "error", err)
		} else {
			limit := 1
			if policy == OverlapBufferOne {
				limit = 2
			}
			if active >= limit {
				return false
			}
		}
	}

	ttl := defaultFireTTL(s.scheduleInterval(sc))
	created, err := s.queue.EnqueueFire(sc, occurrence, occurrence.Add(ttl), fireMaxAttempts)
	if err != nil {
		slog.Error("scheduler: failed to enqueue fire", "id", sc.ID, "label", sc.Label, "error", err)
		return false
	}
	if !created {
		// The same occurrence was already enqueued — a duplicate tick, or a
		// restart replaying the same due row. Not an error, and not a fire.
		slog.Info("scheduler: occurrence already queued", "id", sc.ID, "label", sc.Label)
	}
	s.wakeWorkers()
	return true
}

// RunWorkers starts the queue's workers WITHOUT the schedule tick loop.
//
// The workers lived inside Run, so an agent with scheduling disabled had a
// queue nobody drained: message.turn tasks sat queued forever. That coupling
// contradicts the whole premise — the queue is infrastructure that scheduled
// fires happen to use, not a scheduler component. A headless agent reachable
// only by CLI needs the workers and has no use for the tick.
//
// Blocks until ctx is cancelled, matching Run's contract.
func (s *Scheduler) RunWorkers(ctx context.Context) {
	if s.queue == nil {
		<-ctx.Done()
		return
	}
	slog.Info("worker: queue workers started without scheduler tick", "owner", s.queueOwner, "workers", queueWorkers)
	s.sweepQueue()
	s.runQueueWorkers(ctx, queueWorkers)
	<-ctx.Done()
	s.queueWG.Wait()
}

// runQueueWorkers leases and runs fires until ctx is cancelled.
//
// Workers poll rather than subscribe. The queue is SQLite and the work unit is
// minutes long, so a 5-second poll costs nothing measurable and avoids a
// notification channel that would have to survive the same restarts the queue
// exists to survive.
func (s *Scheduler) runQueueWorkers(ctx context.Context, n int) {
	for i := 0; i < n; i++ {
		s.queueWG.Add(1)
		go func(worker int) {
			defer s.queueWG.Done()
			ticker := time.NewTicker(queuePollInterval)
			defer ticker.Stop()
			// Drain before the first tick. A worker starting up may be facing a
			// queue full of fires reclaimed from the process it replaced —
			// making those wait out a poll interval would add restart latency
			// to exactly the work that already lost time to the restart.
			drain := func() {
				for ctx.Err() == nil && s.leaseAndRunOne(ctx) {
					// Keep going: a burst of due schedules should not trickle
					// out one per poll.
				}
			}
			drain()
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.queueWake:
					drain()
				case <-ticker.C:
					drain()
				}
			}
		}(i)
	}
}

// wakeWorkers nudges a polling worker to look now instead of on its next tick.
//
// Without this a fire waits up to queuePollInterval before it starts. The old
// in-memory dispatcher handed work straight to a mailbox goroutine, so moving
// to the queue quietly added a latency FLOOR to every scheduled fire — measured
// at a flat 5.0s on a production fire on 2026-08-06. The poll remains as the
// safety net that makes reclaimed and not_before work visible.
func (s *Scheduler) wakeWorkers() {
	if s.queueWake == nil {
		return
	}
	select {
	case s.queueWake <- struct{}{}:
	default: // a wake is already pending; the workers are about to look
	}
}

const queuePollInterval = 5 * time.Second

// queueWorkers bounds concurrent fires. Partition keys already serialize within
// a chat, so this only caps how many DIFFERENT chats fire at once — and with
// two agents on one machine, four is already more parallelism than the
// schedules can produce.
const queueWorkers = 4

// leaseAndRunOne runs at most one task of any kind, reporting whether it found
// work. Dispatch is by kind through the registry; this function knows nothing
// about what any particular kind means.
func (s *Scheduler) leaseAndRunOne(ctx context.Context) bool {
	task, err := s.queue.LeaseNext(s.queueOwner, s.jobTimeout+fireLeaseGrace)
	if err != nil {
		slog.Warn("worker: lease failed", "error", err)
		return false
	}
	if task == nil {
		return false
	}

	if task.Attempt > 1 {
		slog.Info("worker: replaying task after interruption",
			"task_id", task.ID, "kind", task.Kind, "attempt", task.Attempt)
	}

	handler, ok := s.handlers[task.Kind]
	if !ok {
		// An unregistered kind is a wiring bug, not a transient failure.
		// Retrying would burn attempts and hide it; fail it once, loudly.
		slog.Error("worker: no handler for kind", "task_id", task.ID, "kind", task.Kind)
		if ferr := s.queue.FailPermanent(task.ID, "no handler registered for kind "+task.Kind); ferr != nil {
			slog.Warn("worker: failed to record missing handler", "task_id", task.ID, "error", ferr)
		}
		return true
	}

	result, herr := handler(ctx, *task)
	if herr != nil {
		terminal, ferr := s.queue.FailFire(task.ID, herr.Error())
		if ferr != nil {
			slog.Warn("worker: failed to record failure", "task_id", task.ID, "error", ferr)
		}
		slog.Warn("worker: task failed", "task_id", task.ID, "kind", task.Kind,
			"terminal", terminal, "attempt", task.Attempt, "error", herr)
		return true
	}
	if err := s.queue.CompleteFire(task.ID, result); err != nil {
		slog.Warn("worker: failed to close task", "task_id", task.ID, "error", err)
	}
	return true
}

// handleScheduleFire is the handler for scheduled fires.
//
// It never returns an error. runJob owns the retry ladder and the ledger and
// returns only once the occurrence is settled one way or another, so reaching
// the end means the fire was HANDLED — whatever the turn's own outcome, which
// job_runs already records. Failing the task here would replay a fire that
// genuinely ran, which is the duplicate-reminder failure mode.
func (s *Scheduler) handleScheduleFire(ctx context.Context, t LeasedTask) (string, error) {
	entry, err := decodeFirePayload(t.Payload)
	if err != nil {
		// Undecodable payload is permanent: the same bytes will not parse on a
		// retry either. Fail it so it stops consuming workers.
		return "", fmt.Errorf("undecodable fire payload: %w", err)
	}
	s.runJob(ctx, entry)
	return "", nil
}

// sweepQueue reclaims dead leases and drops stale work. Called on each tick so
// an expired lease returns to the queue without waiting for a restart.
func (s *Scheduler) sweepQueue() {
	if n, err := s.queue.ReclaimFires(s.queueOwner); err != nil {
		slog.Warn("scheduler: reclaim failed", "error", err)
	} else if n > 0 {
		slog.Info("scheduler: reclaimed abandoned fires", "count", n)
	}
	if n, err := s.queue.ExpireFires(); err != nil {
		slog.Warn("scheduler: expiry sweep failed", "error", err)
	} else if n > 0 {
		slog.Info("scheduler: dropped stale fires", "count", n)
	}
}
