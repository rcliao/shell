package daemon

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// The CLI transport: a way to talk to a RUNNING agent from a terminal.
//
// `shell send` spawns a fresh `claude -p` and so talks to nobody — no session,
// no memory, no skills, none of the agent that actually exists. This path goes
// through the queue like any other message, which means a CLI conversation
// gets the same session, the same context, and the same durability as a
// Telegram one. It is also the first NON-TELEGRAM producer, which is what
// proves the intake design is really transport-agnostic rather than
// Telegram-shaped with the labels filed off.
//
// The sink writes into the task's result rather than holding a connection,
// because the caller is a separate short-lived process that may not be
// attached — or even running — when the turn finishes. Streaming works by
// updating that result as text arrives and letting the caller poll.

const cliTransport = "cli"

// cliSink streams a reply into the task row so a detached caller can read it.
type cliSink struct {
	store  *store.Store
	taskID int64
}

func (c *cliSink) Update(_ context.Context, textSoFar string) error {
	// Best-effort: a failed partial write must not abort a turn whose final
	// result will be written anyway.
	if err := c.store.SetTaskResult(c.taskID, textSoFar); err != nil {
		slog.Debug("cli sink: partial update skipped", "task_id", c.taskID, "error", err)
	}
	return nil
}

func (c *cliSink) Finish(_ context.Context, final string) error {
	return c.store.SetTaskResult(c.taskID, final)
}

func (c *cliSink) Fail(_ context.Context, cause error) {
	// The caller is polling a result field; silence would look like a hang.
	_ = c.store.SetTaskResult(c.taskID, "turn failed: "+cause.Error())
}

// wireMessageTurns registers the message.turn handler, the CLI transport, and
// the runner that executes a turn against the live agent.
func wireMessageTurns(sched *scheduler.Scheduler, br *bridge.Bridge, st *store.Store) {
	sched.RegisterTransport(cliTransport, func(m scheduler.MessageTurn) (scheduler.Sink, error) {
		return &cliSink{store: st, taskID: m.TaskID}, nil
	})

	sched.EnableMessageTurns(func(ctx context.Context, m scheduler.MessageTurn, sink scheduler.Sink) (string, error) {
		// The process layer emits DELTAS; Sink.Update takes the text SO FAR.
		// Passing the delta straight through made each update overwrite the
		// last, so a caller reading the result field saw fragments rather than
		// a growing reply — observed as "li transport workss" on the first live
		// CLI turn. Accumulate here, at the one place that knows both shapes.
		var sb strings.Builder
		var mu sync.Mutex
		// A replay is answering late after an outage. Say so in the prompt —
		// see replayNote for why silence here produces invented excuses.
		text := m.Text
		if m.Attempt > 1 {
			text += replayNote
		}
		resp, err := br.HandleMessageStreaming(ctx, m.ChatID, m.ThreadID, text, m.SenderName, nil, nil,
			func(delta string) {
				mu.Lock()
				sb.WriteString(delta)
				soFar := sb.String()
				mu.Unlock()
				_ = sink.Update(ctx, soFar)
			})
		if err != nil {
			return "", err
		}
		if err := sink.Finish(ctx, resp.Text); err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}
