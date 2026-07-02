package main

import (
	"flag"
	"time"

	"github.com/rcliao/shell/internal/bench"
)

// runCV evaluates every conversation seed against a sandboxed ghost DB and
// emits an AgentReport with the cv block populated. CV is conversation-shape-
// based — it does not depend on a real per-agent memory.db. The `--agent`
// flag is metadata only.
func runCV(args []string) {
	fs := flag.NewFlagSet("cv", flag.ExitOnError)
	agent := fs.String("agent", "synthetic", "label for this run")
	cases := fs.String("cases", "testdata/bench/conversations", "directory of *.yml conversation seeds")
	topK := fs.Int("topk", 5, "top-K retrieved per probe")
	noise := fs.Int("noise", 0, "N unrelated memories to inject per sandbox (simulates real-DB ranker competition)")
	out := fs.String("out", "", "")
	fs.Parse(args)

	cs, err := bench.LoadCVCases(*cases)
	if err != nil {
		die(err)
	}
	rep, err := bench.RunCVWithNoise(cs, *topK, *noise)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *agent, Timestamp: time.Now(), CV: rep}, *out)
}
