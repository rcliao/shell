package project

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Goal schedules (docs/DESIGN-AGENT-LOOPS.md step 2). A project goal is an
// event-mode schedule whose fire runs one step of the goal as a project turn
// in the project's thread. The goal itself lives on the schedule row.

// EventGoalDue is a goal schedule's fire.
const EventGoalDue = "goal.due"

// GoalDedupKey is the explicit dedup key of a project goal's schedule: the
// same goal asked twice for one project returns the running schedule.
func GoalDedupKey(slug, goal string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(goal)))
	return "goal:" + slug + ":" + hex.EncodeToString(sum[:])[:12]
}

// GoalScheduleMessage builds the event envelope for a project goal schedule.
func GoalScheduleMessage(slug string, chatID, threadID int64, goalKey string) (string, error) {
	return eventEnvelope(EventPayload{
		Event: EventGoalDue, Slug: slug, ChatID: chatID, ThreadID: threadID, GoalKey: goalKey,
	})
}

// GoalTurn is what one goal fire tells the agent.
type GoalTurn struct {
	ScheduleID   int64
	Slug, Title  string
	Lang         string
	Goal         string
	DoneCriteria string
	Notes        string
	Run, MaxRuns int
	// LastVerdict and LastReason are the latest ledger row, if any.
	LastVerdict, LastReason string
	LastAt                  time.Time
}

// GoalPrompt renders the prompt for one goal fire: do one step, then report
// it with shell_schedule(progress) — the report is what the next fire sees.
func GoalPrompt(t GoalTurn) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Goal schedule #%d — project %s (%s), run %d of %d]\n", t.ScheduleID, t.Slug, t.Title, t.Run, t.MaxRuns)
	fmt.Fprintf(&b, "Goal: %s\n", t.Goal)
	fmt.Fprintf(&b, "Done when: %s\n", t.DoneCriteria)
	if t.Notes != "" {
		fmt.Fprintf(&b, "Your notes from earlier runs:\n%s\n", t.Notes)
	} else {
		b.WriteString("Your notes from earlier runs: none yet — this is the first step.\n")
	}
	if t.LastVerdict != "" {
		line := t.LastVerdict
		if t.LastReason != "" {
			line += " — " + t.LastReason
		}
		fmt.Fprintf(&b, "Last report (%s): %s\n", t.LastAt.Local().Format("Jan 2 15:04"), line)
	}
	b.WriteString(`
Do ONE step toward the goal now (check, compare, look something up, update the doc), not the whole thing.
Then call shell_schedule(action="progress", id=` + fmt.Sprint(t.ScheduleID) + `, evidence="<what you did and found, with numbers or links>", notes="<everything the next run needs: what was checked, current best, what to try next>"). notes replaces the old notes, so carry forward what still matters.
Reply in this thread only when the family should know something now (a change, a find, a question). If nothing changed, reply [noop].
`)
	if t.Run >= t.MaxRuns {
		b.WriteString("This is the last run: say in the thread where the goal stands and what you suggest next.\n")
	}
	if t.Lang != "" {
		fmt.Fprintf(&b, "Reply in the project's language: %s.\n", t.Lang)
	}
	return b.String()
}

// GoalExpiredLine is the one line sent to the thread when a goal runs out of
// runs without being done.
func GoalExpiredLine(goal, lang string, runs int) string {
	if strings.HasPrefix(lang, "zh") {
		return fmt.Sprintf("⏹ %s：跑了 %d 次還沒達成，先停下來。", goal, runs)
	}
	return fmt.Sprintf("⏹ %s — stopped after %d runs without reaching done.", goal, runs)
}
