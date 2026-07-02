package main

import (
	"flag"
	"time"

	"github.com/rcliao/shell/internal/bench"
)

// runCA scores Convention Adherence for one agent.
func runCA(args []string) {
	fs := flag.NewFlagSet("ca", flag.ExitOnError)
	agent := fs.String("agent", "pikamini", "")
	home := fs.String("home", "", "")
	rules := fs.String("rules", "testdata/bench/conventions", "directory of yaml CA rules")
	limit := fs.Int("limit", 500, "max assistant messages to scan")
	out := fs.String("out", "", "")
	fs.Parse(args)

	t := parseTarget(*agent, *home)
	rs, err := bench.LoadCARules(*rules)
	if err != nil {
		die(err)
	}
	rep, err := bench.ConventionAdherence(t, rs, *limit)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *agent, Timestamp: time.Now(), CA: rep}, *out)
}
