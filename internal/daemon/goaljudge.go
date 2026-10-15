package daemon

import (
	"context"
	"log/slog"
	"time"

	"github.com/rcliao/shell/internal/judge"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/rpc"
	"github.com/rcliao/shell/internal/store"
)

// goalJudgeModel is the DoneJudge's model: a separate, cheaper call than the
// agent's own, with no tools (docs/DESIGN-AGENT-LOOPS.md step 3).
const goalJudgeModel = "sonnet"

// goalJudge wires the DoneJudge for the RPC server: the claude CLI with the
// process manager's child environment, and one usage row (source "judge")
// per call that reached the model.
func goalJudge(st *store.Store, env func() []string) rpc.GoalJudgeFunc {
	run := judge.CLI(goalJudgeModel, env)
	return func(ctx context.Context, goal, criteria, evidence string) (judge.Verdict, error) {
		started := time.Now()
		v, err := judge.Done(ctx, run, judge.DefaultTimeout, goal, criteria, evidence)
		if v.CostUSD > 0 || v.InputTokens > 0 {
			st.LogUsage(0, 0, v.InputTokens, v.OutputTokens, 0, 0, v.CostUSD, 1, "judge", goalJudgeModel,
				0, 0, time.Since(started).Milliseconds(), 0)
		}
		return v, err
	}
}

// goalLessonLimit is how many lessons one goal fire is given.
const goalLessonLimit = 5

// goalLessonRecall is the LessonStore's read side for goal fires (design
// step 4): the project's newest goal lessons, by tag. Nil memory = none.
func goalLessonRecall(mem *memory.Memory) func(ctx context.Context, chatID int64, tag string) []string {
	if mem == nil {
		return nil
	}
	return func(ctx context.Context, chatID int64, tag string) []string {
		ls, err := mem.GoalLessons(ctx, chatID, tag, goalLessonLimit)
		if err != nil {
			slog.Warn("goal: lesson recall failed", "chat_id", chatID, "tag", tag, "error", err)
			return nil
		}
		out := make([]string, 0, len(ls))
		for _, l := range ls {
			out = append(out, l.Content)
		}
		return out
	}
}
