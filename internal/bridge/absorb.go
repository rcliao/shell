package bridge

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/rcliao/shell/internal/process"
)

// InjectFollowUp absorbs a same-sender follow-up into the chat's in-flight
// turn (V2-H46): the text is written to the live subprocess stdin as an
// additional user message, so the model folds the answer into the reply it
// is already composing — instead of the follow-up waiting out the full turn.
//
// Deliberately NO prework (ghost/transcript/Channel B fan-out): the active
// turn already carries this turn's context, and prework latency is exactly
// what absorption is buying back. Returns the active turn's age for
// instrumentation; the error names the refusal reason when the process layer
// judged injection unsafe (see process.ErrInject*).
func (b *Bridge) InjectFollowUp(chatID, threadID int64, text, senderName string) (time.Duration, error) {
	agent := b.resolveAgent(chatID)
	inj, ok := agent.(process.Injector)
	if !ok {
		return 0, fmt.Errorf("agent does not support mid-turn injection")
	}

	msg := text
	if senderName != "" {
		// Same [From: ...] framing as a normal turn, plus an explicit cue that
		// this arrived mid-turn — the model answers both in one reply (no
		// numbered scaffolding needed for the single-message case; bursts of
		// 2+ waiters stay on the H44 coalesce path).
		msg = fmt.Sprintf("[From: %s | follow-up sent while you were working — fold the answer into the reply you are composing]\n%s", senderName, text)
	}

	age, err := inj.InjectUserText(process.SessionKey{ChatID: chatID, ThreadID: threadID}, msg)
	if err != nil {
		return age, err
	}

	// Keep the session transcript honest: the injected message is part of the
	// conversation the subprocess saw.
	if sess, serr := b.store.GetSession(chatID, threadID); serr == nil && sess != nil {
		if lerr := b.store.LogMessage(sess.ID, "user", msg); lerr != nil {
			slog.Warn("absorb: failed to log injected message", "error", lerr, "chat_id", chatID)
		}
	}
	return age, nil
}
