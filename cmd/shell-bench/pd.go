package main

import (
	"flag"
	"time"

	"github.com/rcliao/shell/internal/bench"
)

// runPD scores Persona Distinctness between two agents.
//
// `shell-bench pd --a pikamini --b umbreonmini`
func runPD(args []string) {
	fs := flag.NewFlagSet("pd", flag.ExitOnError)
	a := fs.String("a", "pikamini", "agent A name")
	b := fs.String("b", "umbreonmini", "agent B name")
	home := fs.String("home", "", "")
	// Cycle 60: default 300 (was 1000). Larger windows drag in older messages
	// from role-bleed periods; 300 matches the cycle-50 baseline.
	limit := fs.Int("limit", 300, "max assistant messages per agent")
	topK := fs.Int("topk", 30, "top-K signature tokens per agent")
	holdout := fs.Float64("holdout", 0.2, "test holdout fraction")
	out := fs.String("out", "", "")
	fs.Parse(args)

	ta := parseTarget(*a, *home)
	tb := parseTarget(*b, *home)
	rep, err := bench.PersonaDistinctness(ta, tb, *limit, *topK, *holdout)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *a + "_vs_" + *b, Timestamp: time.Now(), PD: rep}, *out)
}
