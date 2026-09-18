package bridge

import (
	"log/slog"
	"strings"
)

// asideMaxBytes bounds what counts as a pre-tool ASIDE rather than content.
// Every leak observed 9/1–9/16 ("Probably sandbox network. Retry unsandboxed.",
// "Let me check the actual live schedule config…", "That's a status report —
// reply ≤2 sentences", 「讓我確認一下 Boudin 在 Pier 39 到底有沒有店…」) was
// under 125 bytes; an answer the model gives BEFORE saving a memory ("Long
// reply with content." then Write then "Memory saved") is longer and must
// survive — that shape is the regression behind allText. Bytes, not runes:
// Chinese packs a paragraph into ~50 characters (150 bytes), so a rune count
// that admits an English sentence would swallow a Chinese answer.
const asideMaxBytes = 160

// userFacingText decides what a person sees from a turn that mixed prose and
// tool calls. The CLI streams every assistant text block, so a turn like
//
//	"Let me check the schedule first." → tool → "Your reminder is set for 7:00."
//
// arrives as two segments. The first is the agent thinking out loud; it was
// shown live while the tool ran and has no place in the final message. Rule:
// a segment that precedes a tool call is dropped when it is short (an aside)
// AND shorter than the longest segment after it — long pre-tool prose is
// content and stays; a short answer followed by a memory save and "Memory
// saved" stays, because the confirmation is the shorter one; and a turn that
// ends on a tool call keeps whatever it said. Returns the text to deliver and the
// segments it dropped, for the caller to log.
func userFacingText(segments []string) (text string, dropped []string) {
	if len(segments) == 0 {
		return "", nil
	}
	last := len(segments) - 1
	// longestAfter[i] is the longest segment that follows i: an aside is
	// short AND shorter than what comes after it. A short answer followed
	// by "Memory saved" is longer than what follows, so it stays.
	longestAfter := make([]int, len(segments))
	for i := last - 1; i >= 0; i-- {
		n := len(strings.TrimSpace(segments[i+1]))
		if n < longestAfter[i+1] {
			n = longestAfter[i+1]
		}
		longestAfter[i] = n
	}
	var kept []string
	for i, seg := range segments {
		n := len(strings.TrimSpace(seg))
		if i < last && n <= asideMaxBytes && n < longestAfter[i] {
			dropped = append(dropped, seg)
			continue
		}
		kept = append(kept, seg)
	}
	return strings.Join(kept, "\n\n"), dropped
}

// applyUserFacingText rewrites result text for a turn a person will read,
// logging what was cut so the loss is visible. Journal turns keep the full
// narrative. What decides is the DESTINATION, not who started the turn: a
// prompt-mode schedule is system-initiated but its reply is sent to a real
// chat, so it is filtered like a user turn.
func applyUserFacingText(chatID int64, journal bool, source string, segments []string, full string) string {
	if journal {
		return full
	}
	text, dropped := userFacingText(segments)
	if len(dropped) == 0 {
		return full
	}
	for _, d := range dropped {
		slog.Info("reply: dropped pre-tool aside", "chat_id", chatID, "source", source, "aside", head(d, 120))
	}
	return text
}
