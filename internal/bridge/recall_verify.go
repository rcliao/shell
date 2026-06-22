package bridge

import (
	"log/slog"
	"regexp"
	"strings"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

// Runtime recall-grounding verification — the read-side twin of write_verify.go.
//
// A confabulating agent can answer a question about a previously-stored fact
// ("yesterday's dairy total", "what did I log for lunch") straight from lossy
// chat memory, without ever consulting the canonical store — producing stale or
// invented answers. This is the symmetric failure to verbal_save: a verbal
// recall. This module classifies each turn by cross-checking:
//
//   1. did the user ask about a previously-stored fact?   (recall trigger)
//   2. was the answer backed by a real read?              (read tool OR ghost
//      injection supplied by the bridge behind the scenes)
//
// The result is logged to store.recall_verifications so we can measure the
// ungrounded-recall rate over time — and, crucially, how much of the grounding
// ghost carries on its own (GhostCoverage). That tells us whether leaning
// harder on ghost injection is the lever to improve recall.

// recallTriggerRe matches English questions that retrieve a previously-stored
// fact: temporal back-references, totals, and "remember/what did I" forms.
var recallTriggerRe = regexp.MustCompile(`(?i)(\b(yesterday|last (time|week|night)|earlier|so far|remember|recall)\b|\bwhat (did|was|were|have)\b.*\b(i|we|my|log|eat|ate|drink|take|took)\b|\bhow (much|many)\b.*\b(did|have|so far|today|total)\b|\bwhen did\b|\btotal\b.*\b(today|this week|so far)\b)`)

// recallTriggerCJK are Chinese cues that strongly imply retrieval of a logged
// fact. Deliberately excludes bare 多少/幾 (unit/quantity questions like
// "3/4 是多少cm" are not recalls) — we require a temporal/aggregate/recall cue.
var recallTriggerCJK = []string{
	"昨天", "前天", "上次", "之前", "上週", "上周", "這週", "这周", "這禮拜",
	"全天", "總共", "总共", "目前", "到現在", "到目前",
	"還記得", "还记得", "記得我", "记得我", "幾號", "几号", "什麼時候", "什么时候",
	"今天吃", "今天喝", "今天的", "這幾天", "这几天",
}

// isRecallTrigger reports whether the user message asks about a stored past fact.
func isRecallTrigger(userMsg string) bool {
	if recallTriggerRe.MatchString(userMsg) {
		return true
	}
	for _, k := range recallTriggerCJK {
		if strings.Contains(userMsg, k) {
			return true
		}
	}
	return false
}

// isReadTool reports whether a tool call reads from a canonical store (ghost
// memory, Notion, Google Doc, or the food-log skill). Bash calls are inspected
// for known read commands. Writes (ghost_put, notion-create/update) are NOT
// reads and are excluded.
func isReadTool(tc process.ToolCall) bool {
	name := strings.ToLower(tc.Name)
	switch {
	case strings.Contains(name, "ghost_search"),
		strings.Contains(name, "ghost_get"),
		strings.Contains(name, "ghost_context"),
		strings.Contains(name, "ghost_expand"),
		strings.Contains(name, "notion-fetch"),
		strings.Contains(name, "notion-query"),
		strings.Contains(name, "notion-retrieve"):
		return true
	case name == "bash" || strings.HasSuffix(name, "__bash"):
		cmd, _ := tc.Input["command"].(string)
		cmd = strings.ToLower(cmd)
		return strings.Contains(cmd, "food-log") ||
			strings.Contains(cmd, "ghost get") ||
			strings.Contains(cmd, "ghost search") ||
			strings.Contains(cmd, "ghost context")
	}
	return false
}

// recallVerdict is the classified outcome of one turn.
type recallVerdict struct {
	classification string // grounded_recall | memory_recall | ""
	triggered      bool
	readOK         bool
	ghostInjected  bool
	grounding      string // active_read | ghost_inject | none
	toolNames      string
}

// shouldLog is false for turns that aren't recall triggers (the common case).
func (v recallVerdict) shouldLog() bool { return v.classification != "" }

// isMiss reports whether a verdict is an ungrounded recall worth correcting.
func (v recallVerdict) isMiss() bool { return v.classification == "memory_recall" }

// classifyRecall cross-checks the recall trigger against the grounding signals.
//
//   - grounded_recall (active_read):  user asked about a stored fact AND a real
//     read tool succeeded this turn
//   - grounded_recall (ghost_inject): user asked about a stored fact AND the
//     bridge injected ghost memories for this turn (no explicit read needed)
//   - memory_recall:                  recall trigger answered from raw context,
//     no read and no ghost injection — the ungrounded, risky case
//   - "" (skip):                      not a recall trigger
func classifyRecall(userMsg, response string, calls []process.ToolCall, ghostInjected bool) recallVerdict {
	v := recallVerdict{
		triggered:     isRecallTrigger(userMsg),
		ghostInjected: ghostInjected,
	}
	if !v.triggered {
		return v
	}

	var names []string
	for _, tc := range calls {
		if !isReadTool(tc) || tc.Failed {
			continue
		}
		names = append(names, tc.Name)
		v.readOK = true
	}
	v.toolNames = strings.Join(names, ",")

	switch {
	case v.readOK:
		v.classification = "grounded_recall"
		v.grounding = "active_read"
	case v.ghostInjected:
		v.classification = "grounded_recall"
		v.grounding = "ghost_inject"
	default:
		v.classification = "memory_recall"
		v.grounding = "none"
	}
	return v
}

// verifyRecall classifies the turn and logs the verdict to the recall ledger.
// Log-only for now (mirrors how write-hygiene shipped before enforcement).
// Best-effort: never fails the user-facing turn.
func (b *Bridge) verifyRecall(chatID, sessID int64, userMsg, response string, calls []process.ToolCall, ghostInjected bool, source string) {
	v := classifyRecall(userMsg, response, calls, ghostInjected)
	if !v.shouldLog() {
		return
	}

	if v.isMiss() {
		slog.Warn("recall ungrounded",
			"chat_id", chatID, "class", v.classification,
			"ghost_injected", v.ghostInjected, "tools", v.toolNames)
	}

	if err := b.store.LogRecallVerification(store.RecallVerification{
		ChatID:         chatID,
		SessionID:      sessID,
		Classification: v.classification,
		Triggered:      v.triggered,
		ReadOK:         v.readOK,
		GhostInjected:  v.ghostInjected,
		Grounding:      v.grounding,
		ToolNames:      v.toolNames,
		Source:         source,
	}); err != nil {
		slog.Warn("failed to log recall verification", "error", err)
	}
}
