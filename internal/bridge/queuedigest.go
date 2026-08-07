package bridge

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// The queue, as something an agent can perceive.
//
// A durable queue is only worth having if work put on it is later noticed.
// Nothing noticed: message turns and scheduled fires flow through it because
// the daemon drives them, but the one kind an AGENT creates for itself sat
// unused, because seeing it required calling a tool to ask.
//
// This renders the parts an agent can act on. Deliberately narrow: what is
// still owed, and what went wrong. Not throughput, not history — a digest that
// lists everything is one more thing to skim past.

// queueDigestLimit caps each section. Past a handful the digest stops being a
// prompt and starts being a report, and the beat that reads it has other work.
const queueDigestLimit = 5

// failureLookback bounds how far back a failure is still news. A week-old
// expiry has either been dealt with or stopped mattering.
const failureLookback = 24 * time.Hour

func (b *Bridge) queueDigest() string {
	if b.store == nil {
		return ""
	}

	owed := b.owedWork()
	broken := b.recentQueueFailures()
	if len(owed) == 0 && len(broken) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("[Durable work]\n")
	if len(owed) > 0 {
		for _, t := range owed {
			sb.WriteString(fmt.Sprintf("- Task %d (%s): %s%s\n",
				t.ID, t.State, taskSummary(t), dueSuffix(t)))
		}
	}
	for _, t := range broken {
		reason := t.LastError
		if reason == "" {
			reason = "no reason recorded"
		}
		sb.WriteString(fmt.Sprintf("- Task %d %s: %s — %s\n",
			t.ID, t.State, taskSummary(t), truncate(reason, 120)))
	}
	sb.WriteString("[End durable work]\n\n")
	return sb.String()
}

// owedWork is work this agent registered for later that later has not answered.
//
// Only agent-created kinds. A queued message turn is a person waiting on a
// reply the daemon is already handling, and a scheduled fire is the scheduler's
// business — surfacing either would ask the agent to worry about machinery it
// does not drive.
func (b *Bridge) owedWork() []store.Task {
	var out []store.Task
	for _, state := range []string{store.TaskQueued, store.TaskLeased} {
		tasks, err := b.store.ListTasks(state, 50)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.Kind != agentTaskKind {
				continue
			}
			out = append(out, t)
			if len(out) >= queueDigestLimit {
				return out
			}
		}
	}
	return out
}

// recentQueueFailures surfaces work that will NOT happen unless something
// changes. Expired is included alongside failed on purpose: nothing went wrong
// in an expiry, but a beat that never fired and a beat that fired and errored
// are both work the agent expected and did not get.
func (b *Bridge) recentQueueFailures() []store.Task {
	cutoff := time.Now().Add(-failureLookback)
	var out []store.Task
	for _, state := range []string{store.TaskFailed, store.TaskExpired} {
		tasks, err := b.store.ListTasks(state, 50)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if t.DoneAt == nil || t.DoneAt.Before(cutoff) {
				continue
			}
			out = append(out, t)
			if len(out) >= queueDigestLimit {
				return out
			}
		}
	}
	return out
}

// agentTaskKind mirrors scheduler.TaskKindAgent. Duplicated rather than
// imported because the scheduler imports the bridge's siblings and a dependency
// the other way is not worth one constant.
const agentTaskKind = "agent.task"

// taskSummary describes a task the way its author would recognise it.
func taskSummary(t store.Task) string {
	if s := jsonStringField(t.Payload, "title"); s != "" {
		return truncate(s, 100)
	}
	if s := jsonStringField(t.Payload, "prompt"); s != "" {
		return truncate(s, 100)
	}
	return t.Kind
}

// dueSuffix marks work that is scheduled rather than pending now, so "not done
// yet" is not mistaken for "overdue".
func dueSuffix(t store.Task) string {
	if t.NotBefore == nil || !t.NotBefore.After(time.Now()) {
		return ""
	}
	return fmt.Sprintf(" (not before %s)", t.NotBefore.Local().Format("Mon 15:04"))
}

// jsonStringField pulls one string field out of a task payload. The queue
// treats payloads as opaque, so this reads the two fields agent tasks are known
// to carry and stays quiet about anything else.
func jsonStringField(payload, field string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return ""
	}
	s, _ := m[field].(string)
	return s
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
