// shell-bench runs LLM-free benchmarks for shell agents against ghost memory.
//
// Capability dimensions:
//   - wh: Write Hygiene — claimed memos that landed in correct ns
//   - rf: Recall Fidelity — gold-answer presence in retrieved memories
//
// Usage:
//
//	shell-bench wh --agent pikamini --since 7d
//	shell-bench rf --agent pikamini --cases testdata/bench/cases/recall
//	shell-bench all --agent pikamini   (runs both, writes eval.json)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/bench"
)

// ownerChats returns the chat IDs whose user messages count as "real" for
// bench metrics, from SHELL_BENCH_OWNER_CHATS (comma-separated Telegram chat
// IDs). Kept out of source so the public repo doesn't carry personal IDs.
// Unset means owner-scoped metrics match no messages, mirroring the empty
// OwnerChats behavior in internal/bench.
func ownerChats() []int64 {
	raw := os.Getenv("SHELL_BENCH_OWNER_CHATS")
	if raw == "" {
		fmt.Fprintln(os.Stderr, "shell-bench: SHELL_BENCH_OWNER_CHATS unset — owner-scoped metrics will be empty")
		return nil
	}
	var ids []int64
	for _, part := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shell-bench: bad chat id %q in SHELL_BENCH_OWNER_CHATS\n", part)
			os.Exit(2)
		}
		ids = append(ids, id)
	}
	return ids
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "wh":
		runWH(os.Args[2:])
	case "rf":
		runRF(os.Args[2:])
	case "all":
		runAll(os.Args[2:])
	case "migrate-meal-aliases":
		runMigrateMealAliases(os.Args[2:])
	case "proposals":
		runProposals(os.Args[2:])
	case "cv":
		runCV(os.Args[2:])
	case "pick-next":
		runPickNext(os.Args[2:])
	case "pd":
		runPD(os.Args[2:])
	case "ca":
		runCA(os.Args[2:])
	case "si":
		runSI(os.Args[2:])
	case "all-dims":
		runAllDims(os.Args[2:])
	case "topic-stats":
		runTopicStats(os.Args[2:])
	case "focus", "fc":
		runFocus(os.Args[2:])
	case "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `shell-bench — LLM-free agent OS benchmarks

  wh    Write Hygiene  — did memo-style requests persist to ghost?
  rf    Recall Fidelity — does ghost surface the right gold answer?
  all   Run wh + rf, write eval.json

Common flags:
  --agent NAME     (default: pikamini)
  --home PATH      base for ~/.shell/agents/<name>/  (default: $HOME/.shell)
  --since DURATION how far back, e.g. 7d, 24h  (wh only, default 7d)
  --cases PATH     dir of *.yml RF cases  (default: testdata/bench/cases/recall)
  --topk N         top-K retrieved per RF case (default: 5)
  --out PATH       write JSON report  (default: stdout)`)
}

func parseTarget(agent, home string) bench.AgentTarget {
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".shell")
	}
	base := filepath.Join(home, "agents", agent)
	return bench.AgentTarget{
		Name:       agent,
		ShellDB:    filepath.Join(base, "shell.db"),
		MemoryDB:   filepath.Join(base, "memory.db"),
		Namespace:  "agent:" + agent,
		OwnerChats: ownerChats(),
	}
}

func parseSince(spec string) time.Duration {
	if strings.HasSuffix(spec, "d") {
		var n int
		fmt.Sscanf(spec, "%dd", &n)
		return time.Duration(n) * 24 * time.Hour
	}
	d, _ := time.ParseDuration(spec)
	if d == 0 {
		d = 7 * 24 * time.Hour
	}
	return d
}

func runWH(args []string) {
	fs := flag.NewFlagSet("wh", flag.ExitOnError)
	agent := fs.String("agent", "pikamini", "")
	home := fs.String("home", "", "")
	since := fs.String("since", "7d", "")
	out := fs.String("out", "", "")
	fs.Parse(args)

	t := parseTarget(*agent, *home)
	now := time.Now()
	rep, err := bench.WriteHygiene(t, now.Add(-parseSince(*since)), now)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *agent, Timestamp: now, WH: rep}, *out)
}

func runRF(args []string) {
	fs := flag.NewFlagSet("rf", flag.ExitOnError)
	agent := fs.String("agent", "pikamini", "")
	home := fs.String("home", "", "")
	cases := fs.String("cases", "testdata/bench/cases/recall", "")
	topK := fs.Int("topk", 5, "")
	out := fs.String("out", "", "")
	fs.Parse(args)

	t := parseTarget(*agent, *home)
	cs, err := bench.LoadRFCases(*cases)
	if err != nil {
		die(err)
	}
	rep, err := bench.RecallFidelity(t, cs, *topK)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *agent, Timestamp: time.Now(), RF: rep}, *out)
}

func runAll(args []string) {
	fs := flag.NewFlagSet("all", flag.ExitOnError)
	agent := fs.String("agent", "pikamini", "")
	home := fs.String("home", "", "")
	since := fs.String("since", "7d", "")
	cases := fs.String("cases", "testdata/bench/cases/recall", "")
	topK := fs.Int("topk", 5, "")
	out := fs.String("out", "", "")
	fs.Parse(args)

	t := parseTarget(*agent, *home)
	now := time.Now()
	wh, err := bench.WriteHygiene(t, now.Add(-parseSince(*since)), now)
	if err != nil {
		die(err)
	}
	cs, err := bench.LoadRFCases(*cases)
	if err != nil {
		die(err)
	}
	rf, err := bench.RecallFidelity(t, cs, *topK)
	if err != nil {
		die(err)
	}
	emit(&bench.AgentReport{Agent: *agent, Timestamp: now, WH: wh, RF: rf}, *out)
}

func emit(r *bench.AgentReport, path string) {
	buf, _ := json.MarshalIndent(r, "", "  ")
	if path == "" {
		fmt.Println(string(buf))
		return
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		die(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
