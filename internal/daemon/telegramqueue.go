package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// Telegram as a queue producer.
//
// Before this, Telegram had its own replay ledger (pending_turns) and its own
// replay loop that ran once at boot. It worked, but it was a second, weaker
// copy of what the queue already does: text only, no attempt limit, no expiry,
// and no way to see a stuck turn except by reading the table.
//
// The cutover deliberately does NOT move execution. The handler still runs the
// turn inline, holding its own lease. What moves is the record and the replay:
// a Telegram message is now a message.turn task like any other, so the same
// reclaim path that replays a scheduled fire replays a dropped reply, and a TUI
// message and a Telegram message differ only in which sink renders the answer.
//
// Running both ledgers at once would be the dangerous version — two replay
// mechanisms on one message is how a family gets a duplicate — so this
// REPLACES pending_turns rather than sitting beside it, and the kill switch
// picks exactly one.

const telegramTransportName = "telegram"

// telegramTurnTTL bounds how stale a reply may be before answering is worse
// than staying silent. Matches the 30 minutes the old boot replay used, chosen
// then for the same reason: past that, an answer arrives in a conversation
// that has moved on.
const telegramTurnTTL = 30 * time.Minute

// telegramTurnMaxAttempts allows one inline run plus one replay. The inline run
// counts as attempt 1, so this is "answer it, and if a crash eats that, answer
// it once more" — not an open-ended retry of a real inference.
const telegramTurnMaxAttempts = 2

// telegramLeaseGrace is how long the in-process turn's lease is honoured. It
// only matters for a STALLED turn in a live process: a crashed one is detected
// by owner mismatch, because a boot owner carries a start timestamp and so
// differs even across a SIGHUP exec that keeps the PID.
const telegramLeaseGrace = 30 * time.Minute

// telegramTurnKey is the idempotency key for an inbound Telegram message.
//
// Chat id is part of it because Telegram numbers messages PER CHAT — message
// 5012 exists in every conversation the bot is in, so keying on the message id
// alone would make two different people's messages collide and silently drop
// the second.
func telegramTurnKey(chatID int64, msgID int) string {
	return scheduler.MessageTurnKey(telegramTransportName, fmt.Sprintf("%d:%d", chatID, msgID))
}

// queueTurnLedger records inbound Telegram messages as durable tasks the
// handler leases for itself.
type queueTurnLedger struct {
	store *store.Store
	owner string
}

func (q *queueTurnLedger) Begin(chatID, threadID int64, msgID int, senderName, text string) (bool, error) {
	payload, err := json.Marshal(scheduler.MessageTurn{
		Transport:  telegramTransportName,
		ExternalID: fmt.Sprintf("%d:%d", chatID, msgID),
		ChatID:     chatID,
		ThreadID:   threadID,
		SenderName: senderName,
		Text:       text,
	})
	if err != nil {
		return false, fmt.Errorf("encode message turn: %w", err)
	}
	expires := time.Now().UTC().Add(telegramTurnTTL)
	_, doneAlready, err := q.store.BeginOwnedTask(store.Task{
		Kind:           scheduler.TaskKindMessageTurn,
		Source:         store.TaskSourceTelegram,
		IdempotencyKey: telegramTurnKey(chatID, msgID),
		PartitionKey:   scheduler.PartitionKey(chatID, threadID),
		Payload:        string(payload),
		MaxAttempts:    telegramTurnMaxAttempts,
		ExpiresAt:      &expires,
	}, q.owner, telegramLeaseGrace)
	return doneAlready, err
}

func (q *queueTurnLedger) Complete(chatID int64, msgID int) error {
	// No result text: the reply already went to Telegram by the time this
	// runs. Storing it again would duplicate the transcript in a table that
	// exists to track delivery, not to hold conversation.
	return q.store.CompleteOwnedTask(telegramTurnKey(chatID, msgID), "")
}

// Undelivered lets drain block on turns this ledger owns.
//
// Counts every recent message turn, not only Telegram-sourced ones: a CLI turn
// mid-flight is also somebody waiting on a reply, and a restart that discards it
// loses the answer just the same.
func (q *queueTurnLedger) Undelivered(maxAge time.Duration) (int, error) {
	return q.store.CountUndeliveredTurnsSince(scheduler.TaskKindMessageTurn, maxAge)
}

func (q *queueTurnLedger) Abandon(chatID int64, msgID int) error {
	return q.store.AbandonOwnedTask(telegramTurnKey(chatID, msgID),
		"handler could not deliver a reply; requeued for replay", q.owner)
}

// telegramSink delivers a REPLAYED turn.
//
// Update is a no-op on purpose. A live turn streams by editing a placeholder
// the handler created, but a replay has no placeholder — the process that made
// it is gone. So a replayed answer arrives as one fresh message, which is
// exactly what the old boot replay did.
//
// Finish must report a send that did not land. It used to call a
// fire-and-forget send and return nil, so a replay whose reply failed to post
// was marked done and the answer was gone with nothing to show for it
// (observed 2026-11-09: two replayed replies written, both sends timed out,
// both tasks "done"). A replay is also the LAST attempt, so failing the task is
// not enough on its own — Finish retries the send first, because re-running a
// whole inference to recover from a network blip would be the wrong cost.
type telegramSink struct {
	send   func(chatID, threadID int64, text string) error
	chatID int64
	thread int64
	// retryDelays are the waits before each re-send; nil means
	// replyRetryDelays. Tests shorten them.
	retryDelays []time.Duration
}

// replyRetryDelays spans about two minutes: long enough to outlast a dropped
// connection being detected and replaced, short enough that the reply is still
// worth having when it lands.
var replyRetryDelays = []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}

func (t *telegramSink) Update(context.Context, string) error { return nil }

func (t *telegramSink) Finish(ctx context.Context, final string) error {
	if final == "" {
		return nil
	}
	delays := t.retryDelays
	if delays == nil {
		delays = replyRetryDelays
	}
	err := t.send(t.chatID, t.thread, final)
	for i := 0; err != nil && i < len(delays); i++ {
		slog.Warn("replay: reply not delivered, retrying",
			"chat_id", t.chatID, "thread_id", t.thread, "try", i+1, "wait", delays[i], "error", err)
		select {
		case <-ctx.Done():
			return fmt.Errorf("reply not delivered: %w (gave up: %v)", err, ctx.Err())
		case <-time.After(delays[i]):
		}
		err = t.send(t.chatID, t.thread, final)
	}
	if err != nil {
		slog.Error("replay: reply LOST — every send failed",
			"chat_id", t.chatID, "thread_id", t.thread, "tries", len(delays)+1, "chars", len(final), "error", err)
		return fmt.Errorf("reply not delivered after %d tries: %w", len(delays)+1, err)
	}
	return nil
}

func (t *telegramSink) Fail(_ context.Context, cause error) {
	// Deliberately silent to the chat. A replay that fails has already made
	// someone wait; a "sorry, something broke" arriving half an hour late is
	// worse than nothing, and the queue records the failure either way.
	slog.Warn("telegram replay failed", "chat_id", t.chatID, "thread_id", t.thread, "error", cause)
}

// wireTelegramQueue makes Telegram a producer of message.turn and registers the
// sink that renders a replayed reply. send must report failure and must not
// dedup (see outbound.SendReply).
func wireTelegramQueue(sched *scheduler.Scheduler, send func(chatID, threadID int64, text string) error) {
	sched.RegisterTransport(telegramTransportName, func(m scheduler.MessageTurn) (scheduler.Sink, error) {
		return &telegramSink{send: send, chatID: m.ChatID, thread: m.ThreadID}, nil
	})
}

// replayNote tells the model plainly that it is answering late.
//
// Agents confabulate explanations for downtime they cannot perceive — inventing
// routing and connection theories rather than saying they were down. The turn
// has no other way to know it is a replay, so the fact is stated in the prompt.
const replayNote = "\n\n[system note: the daemon was briefly offline and this message is being answered late via automatic replay. If your absence comes up, say honestly that you were down for a bit and are catching up — do not invent routing or connection theories.]"
