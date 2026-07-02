package main

import (
	"flag"
	"time"

	"github.com/rcliao/shell/internal/bench"
)

func runSI(args []string) {
	fs := flag.NewFlagSet("si", flag.ExitOnError)
	agent := fs.String("agent", "pikamini", "")
	home := fs.String("home", "", "")
	rules := fs.String("rules", "testdata/bench/skills", "")
	limit := fs.Int("limit", 1000, "")
	out := fs.String("out", "", "")
	fs.Parse(args)

	t := parseTarget(*agent, *home)
	rs, err := bench.LoadSIRules(*rules)
	if err != nil {
		die(err)
	}
	rep, err := bench.SkillInvocation(t, rs, *limit)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *agent, Timestamp: time.Now(), SI: rep}, *out)
}
