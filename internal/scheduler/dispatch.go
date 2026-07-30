package scheduler

import (
	"context"
	"log/slog"
	"sync"
)

// Key-partitioned job dispatch.
//
// Firing a schedule means running a full Claude turn, which routinely takes
// minutes (8.5 was observed in production). Running those inline on the tick
// goroutine made every due job wait for the one ahead of it: two schedules due
// nine seconds apart were recorded 12 seconds apart, because the second spent
// 89 seconds queued behind the first.
//
// The fix is NOT a free worker pool. Each chat is backed by ONE Claude
// subprocess, so two concurrent turns for the same chat contend for it — the
// thing the user-busy retry ladder exists to paper over. So work is partitioned
// BY CHAT: one worker goroutine per chat, serialized within a chat, parallel
// across chats. Same shape as an actor mailbox or a partitioned task queue.
//
// Consequence worth stating: overlap becomes possible the moment jobs run off
// the tick goroutine, so every job carries an OverlapPolicy and the dispatcher
// enforces it. See policy.go.

// job is one unit of dispatched work.
type job struct {
	entry ScheduleEntry
	run   func(ctx context.Context, sc ScheduleEntry)
}

// dispatcher owns one mailbox goroutine per chat.
type dispatcher struct {
	mu      sync.Mutex
	mailbox map[int64]chan job
	// running tracks schedule IDs with a job in flight, so an overlap policy
	// can be evaluated without waiting on the chat's mailbox.
	running map[int64]bool
	// buffered tracks schedule IDs with one job already queued behind the
	// running one, which is what bounds OverlapBufferOne.
	buffered map[int64]bool

	wg      sync.WaitGroup
	queueLn int
}

func newDispatcher(queueLen int) *dispatcher {
	if queueLen <= 0 {
		queueLen = 16
	}
	return &dispatcher{
		mailbox:  make(map[int64]chan job),
		running:  make(map[int64]bool),
		buffered: make(map[int64]bool),
		queueLn:  queueLen,
	}
}

// submit queues a job on its chat's mailbox, honoring the schedule's overlap
// policy. Returns false when the policy declined to enqueue, so the caller can
// record the skip in the ledger rather than dropping it silently.
func (d *dispatcher) submit(ctx context.Context, sc ScheduleEntry, policy OverlapPolicy, run func(context.Context, ScheduleEntry)) bool {
	d.mu.Lock()

	switch policy {
	case OverlapSkip:
		if d.running[sc.ID] || d.buffered[sc.ID] {
			d.mu.Unlock()
			return false
		}
	case OverlapBufferOne:
		// One may run and one may wait; a third is dropped. Unbounded buffering
		// would let a slow schedule accumulate a backlog it can never drain.
		if d.running[sc.ID] && d.buffered[sc.ID] {
			d.mu.Unlock()
			return false
		}
	}

	if d.running[sc.ID] {
		d.buffered[sc.ID] = true
	} else {
		d.running[sc.ID] = true
	}

	ch, ok := d.mailbox[sc.ChatID]
	if !ok {
		ch = make(chan job, d.queueLn)
		d.mailbox[sc.ChatID] = ch
		d.wg.Add(1)
		go d.worker(ctx, sc.ChatID, ch)
	}
	d.mu.Unlock()

	select {
	case ch <- job{entry: sc, run: run}:
		return true
	case <-ctx.Done():
		d.clear(sc.ID)
		return false
	default:
		// Mailbox full: the chat is badly backed up. Drop rather than block the
		// tick goroutine, and let the caller record it.
		slog.Warn("scheduler: chat mailbox full, dropping fire", "chat_id", sc.ChatID, "id", sc.ID)
		d.clear(sc.ID)
		return false
	}
}

// worker drains one chat's mailbox, one job at a time.
func (d *dispatcher) worker(ctx context.Context, chatID int64, ch chan job) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-ch:
			j.run(ctx, j.entry)
			d.clear(j.entry.ID)
		}
	}
}

// clear releases a schedule's in-flight/buffered slots. A buffered job is
// promoted to running rather than both flags dropping, so a schedule with work
// still queued keeps looking busy to the overlap check.
func (d *dispatcher) clear(scheduleID int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.buffered[scheduleID] {
		delete(d.buffered, scheduleID)
		d.running[scheduleID] = true
		return
	}
	delete(d.running, scheduleID)
}

// isRunning reports whether a schedule has a job in flight. Test hook.
func (d *dispatcher) isRunning(scheduleID int64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.running[scheduleID]
}

// idle reports whether no job is in flight or queued anywhere.
func (d *dispatcher) idle() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running) == 0 && len(d.buffered) == 0
}

// wait blocks until every worker has exited. Workers exit on ctx cancellation.
func (d *dispatcher) wait() { d.wg.Wait() }
