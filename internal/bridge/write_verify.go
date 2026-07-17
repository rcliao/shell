package bridge

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

// Runtime write-hygiene verification.
//
// A conversational agent can tell the user it persisted something ("saved to
// Notion ✅", "noted it down") without a real write ever happening — pure
// confabulation, because the agent often lacks, or fails to call, the write
// tool. This module classifies each turn by cross-checking three signals that
// are all available at response time:
//
//   1. did the user ask to persist something?     (write trigger)
//   2. did the agent's prose claim a save?         (write claim)
//   3. did a real persistence tool call succeed?   (process.ToolCall + Failed)
//
// The result is logged to store.write_verifications so we can measure the
// confabulation rate over time and prove the (future) enforcement loop helps.
// This is the runtime twin of internal/bench/write_hygiene.go — keep the
// trigger patterns roughly in sync.

// writeTriggerRe mirrors bench/write_hygiene's memoTriggerRe. ASCII triggers
// use word boundaries so "memo" doesn't match "memory"/"memorable".
var writeTriggerRe = regexp.MustCompile(`(?i)(\b|[^\p{L}\p{N}])(memo|remember|save this|log this)([^\p{L}\p{N}]|\b)`)

// writeTriggerCJK are distinctive enough to match as substrings.
var writeTriggerCJK = []string{"記下", "記錄", "幫我記", "記一下", "存起來"}

// isWriteTrigger reports whether the user message asked the agent to persist.
func isWriteTrigger(userMsg string) bool {
	if writeTriggerRe.MatchString(userMsg) {
		return true
	}
	for _, k := range writeTriggerCJK {
		if strings.Contains(userMsg, k) {
			return true
		}
	}
	return false
}

// writeClaimRe_CJK matches Chinese persistence claims at the language level:
// a persistence verb (record / save / write / fill-in / update / register)
// followed by a resultative or directional particle that signals completion.
// This is vocabulary-level, not tied to any user or deployment — agents
// improvise phrasing, so a verb×particle pattern generalizes far better than an
// enumerated phrase list. Particle sets are curated per verb to avoid common
// false positives that share a character (e.g. 存在 "exist", 記得 "remember"
// are excluded because 在/得 are not persistence resultatives here).
var writeClaimRe_CJK = regexp.MustCompile(
	`記(進|下|好|錄|了)|存(進|到|好|起來)|寫(進|到|好)|補(進|上|好)|更新(好|到|進|了)|加(進|到)|建好|登記|登錄`)

// writeClaimRe catches English persistence claims and explicit Notion/doc cues.
var writeClaimRe = regexp.MustCompile(`(?i)\b(logged|saved (it|this|that)|added (it|this|that)? ?to (notion|the (doc|log|database))|recorded (it|this)|noted (it|this) down|wrote (it|this) (to|into))\b`)

// A CJK persistence verb alone is NOT a claim: the same vocabulary appears in
// offers ("要不要我加進去嗎"), promises ("澆完我再補進 Notion"), refusals
// ("還沒吃就不用先記錄"), advice ("記下當天吃了什麼"), past references
// ("那時就寫好了"), and plain non-persistence usage ("加到咖啡裡",
// "熱量來源補上"). A week of production ledger data showed 14/15 verbal_save
// rows were these, not confabulations — each one burning a correction turn.
// So a clause only counts as a claim when it (a) matches the verb pattern,
// (b) carries a completion marker (已/了/好/✅ — every genuine claim sampled
// had one), and (c) has no modal/future/offer/negation/past-reference token.
// This trades a few false negatives (e.g. 了+會 in one clause) for killing
// the dominant false-positive classes; FPs are costlier here.
var cjkCompletionRe = regexp.MustCompile(`已|了|好|✅`)

var cjkClaimGuards = []string{
	// negation / refusal
	"不", "沒", "別",
	// modal / future / offer / advice
	"可以", "能", "會", "再", "等", "嗎", "要不要", "要我", "想", "應該", "記得",
	// embedded question words — advice to the user ("記下曬了多久、吃了
	// 什麼"), never present in a genuine completed-write claim
	"什麼", "多久", "怎麼", "哪",
	// past reference (an earlier turn's write, not this one)
	"那時", "當時", "之前", "上次", "今早", "早上就", "那筆", "那次",
	// coincidence adverbs — "這張近拍剛好補上一個重點" is discussion, not a
	// write, and the 好 inside 剛好 falsely satisfies the completion-marker
	// check (7/16 production FP)
	"剛好", "正好",
}

// cjkClauseSplit breaks a response into clauses so guard tokens in one clause
// don't veto a genuine claim in another ("已補進 Notion 了，之後不會漏").
var cjkClauseSplit = regexp.MustCompile(`[。！？!?\n，,；;]`)

// claimsWrite reports whether the agent's prose asserts a persistence happened.
// peerNames are the configured peer agents' names/aliases: a clause naming a
// peer as the actor ("Umbreon 已經幫妳寫進 Notion 了") reports ANOTHER agent's
// write, which this agent's own tool ledger can never verify — guarded like
// the other non-self-claim classes (7/16 production FP; the peer had in fact
// written, with read-back). Self name/aliases must NOT be passed in.
func claimsWrite(response string, peerNames []string) bool {
	if writeClaimRe.MatchString(response) {
		return true
	}
	for _, clause := range cjkClauseSplit.Split(response, -1) {
		if !writeClaimRe_CJK.MatchString(clause) {
			continue
		}
		if !cjkCompletionRe.MatchString(clause) {
			continue
		}
		guarded := false
		for _, g := range cjkClaimGuards {
			if strings.Contains(clause, g) {
				guarded = true
				break
			}
		}
		if !guarded {
			lower := strings.ToLower(clause)
			for _, p := range peerNames {
				if p != "" && strings.Contains(lower, strings.ToLower(p)) {
					guarded = true
					break
				}
			}
		}
		if !guarded {
			return true
		}
	}
	return false
}

// isPersistenceTool reports whether a tool call is a durable write (memory,
// Notion, Google Doc, or the shell-remember/meal-log skills). Bash calls are
// inspected for known write commands.
func isPersistenceTool(tc process.ToolCall) bool {
	name := strings.ToLower(tc.Name)
	switch {
	case strings.Contains(name, "ghost_put"),
		strings.Contains(name, "ghost_consolidate"),
		strings.Contains(name, "notion-create"),
		strings.Contains(name, "notion-update"),
		strings.Contains(name, "shell_meal_log"),
		strings.Contains(name, "shell_remember"):
		return true
	case name == "bash" || strings.HasSuffix(name, "__bash"):
		cmd, _ := tc.Input["command"].(string)
		cmd = strings.ToLower(cmd)
		if strings.Contains(cmd, "shell-remember") ||
			strings.Contains(cmd, "ghost put") ||
			strings.Contains(cmd, "gog docs") {
			return true
		}
		// The notion skill script: only WRITE subcommands are persistence
		// (get-page / query-db are reads and must not verify a save claim).
		if strings.Contains(cmd, "notion") {
			return strings.Contains(cmd, "patch-prop") ||
				strings.Contains(cmd, "append") ||
				strings.Contains(cmd, "create") ||
				strings.Contains(cmd, "curl")
		}
		return false
	}
	return false
}

// writeVerdict is the classified outcome of one turn.
type writeVerdict struct {
	classification string // verified | verbal_save | silent_failure | unclaimed_trigger | ""
	triggered      bool
	claimed        bool
	writeOK        bool
	writeFailed    bool
	toolNames      string
}

// shouldLog is false for turns with no persistence relevance (the common case).
func (v writeVerdict) shouldLog() bool { return v.classification != "" }

// classifyWrite cross-checks the three signals and returns a verdict.
//
// Classification precedence:
//   - silent_failure:    a persistence tool ran but errored, and none succeeded
//   - verbal_save:       prose claimed a save but no successful write tool ran
//   - verified:          a claim or trigger AND a successful write tool ran
//   - unclaimed_trigger: user asked to persist, agent neither claimed nor wrote
//   - "" (skip):         nothing persistence-related happened
func classifyWrite(userMsg, response string, calls []process.ToolCall, peerNames []string) writeVerdict {
	v := writeVerdict{
		triggered: isWriteTrigger(userMsg),
		claimed:   claimsWrite(response, peerNames),
	}

	var names []string
	for _, tc := range calls {
		if !isPersistenceTool(tc) {
			continue
		}
		names = append(names, tc.Name)
		if tc.Failed {
			v.writeFailed = true
		} else {
			v.writeOK = true
		}
	}
	v.toolNames = strings.Join(names, ",")

	switch {
	case v.writeFailed && !v.writeOK:
		v.classification = "silent_failure"
	case v.claimed && !v.writeOK:
		v.classification = "verbal_save"
	case (v.claimed || v.triggered) && v.writeOK:
		v.classification = "verified"
	case v.triggered && !v.claimed && !v.writeOK:
		v.classification = "unclaimed_trigger"
	default:
		v.classification = ""
	}
	return v
}

// isMiss reports whether a verdict is a write-hygiene failure worth correcting.
func (v writeVerdict) isMiss() bool {
	return v.classification == "verbal_save" || v.classification == "silent_failure"
}

const writeCorrectionPrompt = `[system: write-verification] You just told the user you saved / logged / recorded / 補進 / 記下 something, but no successful write tool call happened this turn — so nothing was actually persisted. Do NOT reply conversationally or re-explain. Actually perform the write NOW using your real tools (ghost_put, scripts/shell-remember, or the Notion / Google-Doc tool). Then reply with a brief, honest confirmation IN THE USER'S LANGUAGE, including the real memory id or page URL as proof. If you genuinely cannot write it, tell the user plainly that it did NOT save and why — never claim success you can't back with a tool result.`

// verifyWriteHygiene classifies the turn, optionally issues a bounded
// correction turn (when enforcement is on and a write claim wasn't backed by a
// real successful tool call), logs the verdict to the ledger, and returns the
// possibly-augmented response. Best-effort: never fails the user-facing turn.
func (b *Bridge) verifyWriteHygiene(ctx context.Context, agent process.Agent, chatID, threadID, sessID int64, userMsg string, resp AgentResponse, result process.SendResult, source string) AgentResponse {
	v := classifyWrite(userMsg, resp.Text, result.ToolCalls, b.peerNameList())
	// Cross-turn carryover: a claim that references the PREVIOUS turn's
	// successful write is honest, not confabulated ("晚餐已經寫進Notion囉"
	// explaining why the last turn was slow — flagged 7/14 15:52 and burned
	// a correction turn). If this chat completed a successful persistence
	// write within the last 3 minutes, downgrade verbal_save to verified.
	if v.classification == "verbal_save" && b.recentWriteOK(chatID, 3*time.Minute) {
		v.classification = "verified"
		v.toolNames = "carryover"
	}
	if v.writeOK {
		b.markWriteOK(chatID)
	}
	if !v.shouldLog() {
		return resp
	}

	enforced := false
	if v.isMiss() {
		slog.Warn("write-hygiene miss",
			"chat_id", chatID, "class", v.classification,
			"claimed", v.claimed, "triggered", v.triggered,
			"write_ok", v.writeOK, "write_failed", v.writeFailed, "tools", v.toolNames)

		if b.claudeCfg.WriteVerifyEnforce && agent != nil {
			corr, err := agent.Send(ctx, process.AgentRequest{
				ChatID:          chatID,
				MessageThreadID: threadID,
				SessionID:       result.SessionID,
				Text:            writeCorrectionPrompt,
				Model:           b.claudeCfg.ResolveModel("conversation"),
			}, nil)
			if err != nil {
				slog.Warn("write-hygiene correction turn failed", "chat_id", chatID, "error", err)
			} else {
				enforced = true
				corrText := stripDirectives(strings.TrimSpace(corr.Text))
				corrText = b.parseArtifacts(corrText, &resp.Photos, &resp.Videos)
				if corrText != "" {
					if resp.Text != "" {
						resp.Text += "\n\n" + corrText
					} else {
						resp.Text = corrText
					}
				}
				// Re-classify against the correction turn's tool calls so the
				// ledger records the post-correction outcome (ideally verified).
				v = classifyWrite(userMsg, resp.Text, append(append([]process.ToolCall{}, result.ToolCalls...), corr.ToolCalls...), b.peerNameList())
				slog.Info("write-hygiene corrected", "chat_id", chatID, "post_class", v.classification, "tools", v.toolNames)
			}
		}
	}

	if err := b.store.LogWriteVerification(store.WriteVerification{
		ChatID:         chatID,
		SessionID:      sessID,
		Classification: v.classification,
		Triggered:      v.triggered,
		Claimed:        v.claimed,
		WriteOK:        v.writeOK,
		WriteFailed:    v.writeFailed,
		ToolNames:      v.toolNames,
		Enforced:       enforced,
		Source:         source,
	}); err != nil {
		slog.Warn("failed to log write verification", "error", err)
	}
	return resp
}

// recentWriteOK reports whether this chat completed a successful persistence
// write within the window (cross-turn carryover for write-verify).
func (b *Bridge) recentWriteOK(chatID int64, window time.Duration) bool {
	b.lastWriteMu.Lock()
	defer b.lastWriteMu.Unlock()
	t, ok := b.lastWriteOK[chatID]
	return ok && time.Since(t) < window
}

func (b *Bridge) markWriteOK(chatID int64) {
	b.lastWriteMu.Lock()
	defer b.lastWriteMu.Unlock()
	if b.lastWriteOK == nil {
		b.lastWriteOK = make(map[int64]time.Time)
	}
	b.lastWriteOK[chatID] = time.Now()
}

// peerNameList flattens configured peer agents into name+alias guard tokens
// for claimsWrite. Never includes this agent's own name.
func (b *Bridge) peerNameList() []string {
	var out []string
	for _, p := range b.peerAgents {
		if p.Name != "" {
			out = append(out, p.Name)
		}
		out = append(out, p.Aliases...)
	}
	return out
}
