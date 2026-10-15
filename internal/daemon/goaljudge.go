package daemon

import (
	"context"
	"time"

	"github.com/rcliao/shell/internal/judge"
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
