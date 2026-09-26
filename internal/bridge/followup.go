package bridge

import (
	"context"
	"log/slog"

	"github.com/rcliao/shell/internal/process"
)

// HandleUnsolicitedTurn delivers a turn the CLI produced on its own inside a
// chat's persistent process — typically a background Agent subagent finishing
// after the agent already replied "I'll look it up and get back to you". The
// process layer used to leave such a turn buffered in the pipe, where the next
// user message consumed it as its answer (2026-09-05, every reply one message
// behind). Now the pump hands it here and it becomes the follow-up message the
// family was promised.
//
// It runs the ordinary response pipeline (noop markers, artifacts, message
// log, shared transcript, usage) with no user message and source "followup",
// then pushes the result to the chat/thread over the transport. Photos are
// subject to the media gate like any other unprompted send.
func (b *Bridge) HandleUnsolicitedTurn(key process.SessionKey, result process.SendResult) {
	chatID, threadID := key.ChatID, key.ThreadID
	if b.store == nil {
		slog.Warn("follow-up dropped: no store", "chat_id", chatID, "thread_id", threadID)
		return
	}
	sess, err := b.store.GetSession(chatID, threadID)
	if err != nil || sess == nil {
		slog.Warn("follow-up dropped: no session row", "chat_id", chatID, "thread_id", threadID, "error", err)
		return
	}
	// The session key's thread is the SESSION's: a lane session (R1) has a
	// negative one. Everything below talks to the chat or the transcript,
	// so it uses the real thread.
	threadID = b.realThread(chatID, threadID)
	model := resolveExecutionProfile(b.claudeCfg, turnKind{chatID: chatID}).Model
	if noopMarkerRe.MatchString(result.Text) {
		// The agent chose silence after its background work (nothing worth
		// saying). processResponse would blank the text and then substitute
		// a tool summary — right for a user turn whose caller suppresses it,
		// wrong here where the text goes straight to the chat. Journal usage
		// and stop.
		resp := b.processResponse(context.Background(), chatID, threadID, sess.ID, "", false, result, "followup", model, "")
		slog.Info("follow-up was [noop], not delivered", "chat_id", chatID, "thread_id", threadID, "tool_calls", len(result.ToolCalls), "chars_after", len(resp.Text))
		return
	}
	resp := b.processResponse(context.Background(), chatID, threadID, sess.ID, "", false, result, "followup", model, "")

	if b.transport == nil {
		slog.Warn("follow-up dropped: no transport", "chat_id", chatID, "thread_id", threadID, "chars", len(resp.Text))
		return
	}
	if resp.Text == "" && len(resp.Photos) == 0 && len(resp.Videos) == 0 && len(resp.Documents) == 0 {
		// A [noop] continuation or an empty result: journaled, nothing to send.
		slog.Info("follow-up empty after processing, not delivered", "chat_id", chatID, "thread_id", threadID, "tool_calls", len(result.ToolCalls))
		return
	}
	if resp.Text != "" {
		b.transport.Notify(chatID, threadID, resp.Text)
	}
	for _, ph := range resp.Photos {
		b.transport.SendPhoto(chatID, threadID, ph.Data, ph.Caption)
	}
	for _, v := range resp.Videos {
		b.transport.SendVideo(chatID, threadID, v.Data, v.Caption)
	}
	for _, d := range resp.Documents {
		if err := b.transport.SendDocument(chatID, threadID, d.Path, d.Caption); err != nil {
			slog.Warn("follow-up document send failed", "chat_id", chatID, "path", d.Path, "error", err)
		}
	}
	slog.Info("follow-up delivered", "chat_id", chatID, "thread_id", threadID,
		"chars", len(resp.Text), "photos", len(resp.Photos), "tool_calls", len(result.ToolCalls))
}
