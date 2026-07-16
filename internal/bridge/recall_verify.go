package bridge

import (
	"log/slog"
	"regexp"
	"strings"
	"time"

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

// cjkStopwords are CJK fragments stripped before judging what a recall
// question is ABOUT: temporal cues, aggregates, and generic verbs/particles.
// What remains after stripping is the subject (e.g. 奶製品 in 今天奶製品總共多少).
var cjkStopwords = []string{
	"昨天", "前天", "今天", "上次", "之前", "上週", "上周", "這週", "这周", "這禮拜",
	"全天", "總共", "总共", "目前", "到現在", "到目前", "還記得", "还记得",
	"記得", "记得", "幾號", "几号", "什麼時候", "什么时候", "這幾天", "这几天",
	"多少", "什麼", "什么", "怎麼", "怎么", "哪些", "有沒有", "有没有",
	"吃了", "喝了", "吃", "喝", "的", "了", "是", "我", "你", "妳", "嗎", "吗", "呢",
}

// enStopwords are English words that carry the recall FORM, not its subject.
var enStopwords = map[string]bool{
	"what": true, "did": true, "was": true, "were": true, "have": true, "has": true,
	"how": true, "much": true, "many": true, "total": true, "today": true,
	"yesterday": true, "last": true, "week": true, "night": true, "time": true,
	"earlier": true, "far": true, "when": true, "remember": true, "recall": true,
	"the": true, "and": true, "for": true, "log": true, "logged": true, "eat": true,
	"ate": true, "drink": true, "drank": true, "take": true, "took": true,
	"this": true, "that": true, "you": true, "your": true,
}

var enWordRe = regexp.MustCompile(`(?i)\b[a-z]{3,}\b`)
var cjkRunRe = regexp.MustCompile(`\p{Han}+`)

// salientTokens extracts the subject tokens of a recall question — the parts
// that name WHAT is being recalled, with temporal/aggregate/form words removed.
func salientTokens(userMsg string) []string {
	var out []string
	for _, w := range enWordRe.FindAllString(userMsg, -1) {
		lw := strings.ToLower(w)
		if !enStopwords[lw] {
			out = append(out, lw)
		}
	}
	for _, run := range cjkRunRe.FindAllString(userMsg, -1) {
		for _, sw := range cjkStopwords {
			run = strings.ReplaceAll(run, sw, "\x00")
		}
		for _, piece := range strings.Split(run, "\x00") {
			if len([]rune(piece)) >= 2 {
				out = append(out, piece)
			}
		}
	}
	return out
}

// injectionCoversQuery reports whether the injected ghost context plausibly
// contains the fact being recalled: at least one subject token of the question
// appears in the injected text. When the question has no extractable subject
// (pure temporal form like 今天吃了多少), we cannot judge relevance and
// conservatively treat the injection as covering.
func injectionCoversQuery(userMsg, injectedText string) bool {
	toks := salientTokens(userMsg)
	if len(toks) == 0 {
		return true
	}
	lower := strings.ToLower(injectedText)
	for _, tok := range toks {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// recallVerdict is the classified outcome of one turn.
type recallVerdict struct {
	classification string // grounded_recall | memory_recall | ""
	triggered      bool
	readOK         bool
	ghostInjected  bool
	grounding      string // active_read | ghost_inject | inject_irrelevant | none
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
//     injected ghost context actually mentions the fact's subject
//   - memory_recall (inject_irrelevant): ghost injected SOMETHING, but nothing
//     about the recalled subject — presence isn't relevance. Before this check
//     the miss path could never fire: injection runs on essentially every turn,
//     so 100% of recalls graded "grounded" and the ledger measured nothing.
//   - memory_recall (none):           no read, no injection at all
//   - "" (skip):                      not a recall trigger
func classifyRecall(userMsg, response string, calls []process.ToolCall, injectedText string) recallVerdict {
	v := recallVerdict{
		triggered:     isRecallTrigger(userMsg),
		ghostInjected: injectedText != "",
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
	case v.ghostInjected && injectionCoversQuery(userMsg, injectedText):
		v.classification = "grounded_recall"
		v.grounding = "ghost_inject"
	case v.ghostInjected:
		v.classification = "memory_recall"
		v.grounding = "inject_irrelevant"
	default:
		v.classification = "memory_recall"
		v.grounding = "none"
	}
	return v
}

// verifyRecall classifies the turn and logs the verdict to the recall ledger.
// Log-only for now (mirrors how write-hygiene shipped before enforcement).
// Best-effort: never fails the user-facing turn.
func (b *Bridge) verifyRecall(chatID, sessID int64, userMsg, response string, calls []process.ToolCall, injectedText string, source string) {
	v := classifyRecall(userMsg, response, calls, injectedText)
	// Cross-turn carryover: a follow-up question about facts the agent read
	// moments ago ("所以這盆是因為…?" right after a Notion diary read) is
	// grounded by THAT read — the ledger just can't see it this turn. Same
	// pattern as write-verify's recentWriteOK (7/16 FP audit: 3/3 sampled
	// ungrounded flags were this or domain-knowledge triggers).
	if v.isMiss() && b.recentReadOK(chatID, 3*time.Minute) {
		v.classification = "grounded_recall"
		v.grounding = "read_carryover"
	}
	if v.readOK {
		b.markReadOK(chatID)
	}
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

// recentReadOK reports whether this chat completed a successful read within
// the window (cross-turn carryover for recall-verify).
func (b *Bridge) recentReadOK(chatID int64, window time.Duration) bool {
	b.lastReadMu.Lock()
	defer b.lastReadMu.Unlock()
	t, ok := b.lastReadOK[chatID]
	return ok && time.Since(t) < window
}

func (b *Bridge) markReadOK(chatID int64) {
	b.lastReadMu.Lock()
	defer b.lastReadMu.Unlock()
	if b.lastReadOK == nil {
		b.lastReadOK = make(map[int64]time.Time)
	}
	b.lastReadOK[chatID] = time.Now()
}
