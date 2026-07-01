package bridge

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

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

// claimsWrite reports whether the agent's prose asserts a persistence happened.
func claimsWrite(response string) bool {
	return writeClaimRe.MatchString(response) || writeClaimRe_CJK.MatchString(response)
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
		return strings.Contains(cmd, "shell-remember") ||
			strings.Contains(cmd, "ghost put") ||
			strings.Contains(cmd, "gog docs") ||
			strings.Contains(cmd, "notion")
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
func classifyWrite(userMsg, response string, calls []process.ToolCall) writeVerdict {
	v := writeVerdict{
		triggered: isWriteTrigger(userMsg),
		claimed:   claimsWrite(response),
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
	v := classifyWrite(userMsg, resp.Text, result.ToolCalls)
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
				v = classifyWrite(userMsg, resp.Text, append(append([]process.ToolCall{}, result.ToolCalls...), corr.ToolCalls...))
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
