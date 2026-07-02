package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	memory "github.com/rcliao/ghost"
)

// pickNext emits the autonomous loop's next-action decision as JSON.
//
// Priority order (first match wins):
//  1. drain-proposal — any open proposal in loop:proposals across agent DBs
//  2. close-goal-gap — current_goal not met and gap >= 0.05
//  3. cv-regression  — cv pass_rate dropped >0.1 from last cycle
//  4. idle           — nothing to do; sleep
//
// Also enforces guard rails:
//  - if state.halted=true → emit halt
//  - if state.idle_streak >= 5 → emit halt
//  - if state.consecutive_failures >= 3 → emit halt
//
// Output schema (always JSON to stdout):
//
//	{
//	  "action": "drain-proposal|close-goal-gap|cv-regression|idle|halt",
//	  "reason": "<one sentence>",
//	  "payload": {...},
//	  "guard_rail_triggered": false
//	}
func runPickNext(args []string) {
	fs := flag.NewFlagSet("pick-next", flag.ExitOnError)
	statePath := fs.String("state", ".evolve/state.json", "")
	agentDBs := fs.String("dbs",
		"/Users/pikamini/.shell/agents/pikamini/memory.db,/Users/pikamini/.shell/agents/umbreonmini/memory.db",
		"")
	fs.Parse(args)

	state := readState(*statePath)
	emit := func(action, reason string, payload any, guard bool) {
		out := map[string]any{
			"action":               action,
			"reason":               reason,
			"payload":              payload,
			"guard_rail_triggered": guard,
			"decided_at":           time.Now().Format(time.RFC3339),
		}
		buf, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(buf))
	}

	// Guard rails first.
	if halted, _ := state["halted"].(bool); halted {
		emit("halt", "state.halted=true", nil, true)
		return
	}
	if streak, ok := state["idle_streak"].(float64); ok && streak >= 5 {
		emit("halt", fmt.Sprintf("idle_streak=%v ≥ 5", streak), nil, true)
		return
	}
	if cf, ok := state["consecutive_failures"].(float64); ok && cf >= 3 {
		emit("halt", fmt.Sprintf("consecutive_failures=%v ≥ 3", cf), nil, true)
		return
	}

	// 1. Drain proposals first.
	type prop struct {
		Agent      string `json:"agent"`
		Key        string `json:"key"`
		Content    string `json:"content"`
		TargetDim  string `json:"target_dim"`
	}
	var open []prop
	for _, db := range strings.Split(*agentDBs, ",") {
		db = strings.TrimSpace(db)
		store, err := memory.NewSQLiteStore(db)
		if err != nil {
			continue
		}
		ms, err := store.List(context.Background(), memory.ListParams{
			NS:    "loop:proposals",
			Limit: 100,
		})
		if err != nil {
			continue
		}
		for _, m := range ms {
			if !strings.Contains(m.Content, "status: open") {
				continue
			}
			td := extractField(m.Content, "target-dim")
			open = append(open, prop{Agent: guessAgentFromPath(db), Key: m.Key, Content: m.Content, TargetDim: td})
		}
	}
	if len(open) > 0 {
		sort.Slice(open, func(i, j int) bool { return open[i].Key < open[j].Key })
		emit("drain-proposal",
			fmt.Sprintf("%d open proposal(s); picking %s", len(open), open[0].Key),
			open[0], false)
		return
	}

	// 2. Goal gap.
	goal, _ := state["current_goal"].(map[string]any)
	if goal != nil {
		target, _ := goal["target"].(float64)
		current, _ := goal["current"].(float64)
		dim, _ := goal["dim"].(string)
		agent, _ := goal["agent"].(string)
		if target-current >= 0.05 {
			emit("close-goal-gap",
				fmt.Sprintf("%s @ %s at %.3f, target %.3f, gap %.3f", dim, agent, current, target, target-current),
				map[string]any{
					"dim": dim, "agent": agent,
					"current": current, "target": target,
					"gap": target - current,
				},
				false)
			return
		}
	}

	// 3. CV regression — compare bench_latest cv vs prior recorded.
	// (Simple version: just flag if pass_rate < 0.85.)
	if bl, ok := state["bench_latest"].(map[string]any); ok {
		if cv, ok := bl["cv_synthetic"].(map[string]any); ok {
			if pr, ok := cv["pass_rate"].(float64); ok && pr < 0.85 {
				emit("cv-regression",
					fmt.Sprintf("cv_synthetic.pass_rate=%.3f below 0.85", pr),
					map[string]any{"pass_rate": pr},
					false)
				return
			}
		}
	}

	// 4. Idle.
	emit("idle", "no open proposals, goal met or absent, cv stable", nil, false)
}

func readState(path string) map[string]any {
	buf, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	json.Unmarshal(buf, &m)
	return m
}

func extractField(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}
