package telegram

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rcliao/teeny-relay/internal/bridge"
)

const maxMessageLength = 4096

type Handler struct {
	auth   *Auth
	bridge *bridge.Bridge
}

func NewHandler(auth *Auth, br *bridge.Bridge) *Handler {
	return &Handler{auth: auth, bridge: br}
}

func (h *Handler) HandleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil {
		return
	}
	if !h.auth.IsAllowed(msg.From.ID) {
		slog.Warn("unauthorized user", "user_id", msg.From.ID, "username", msg.From.Username)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Unauthorized. Your user ID is not in the allowlist.",
		})
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	// Send typing indicator periodically while processing
	typingCtx, typingCancel := context.WithCancel(ctx)
	defer typingCancel()
	go h.sendTypingPeriodically(typingCtx, b, msg.Chat.ID)

	response, err := h.bridge.HandleMessage(ctx, msg.Chat.ID, text)
	typingCancel()

	if err != nil {
		slog.Error("bridge handle message failed", "error", err, "chat_id", msg.Chat.ID)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Error: " + err.Error(),
		})
		return
	}

	h.sendChunked(ctx, b, msg.Chat.ID, response)
}

func (h *Handler) HandleCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil {
		return
	}
	if !h.auth.IsAllowed(msg.From.ID) {
		slog.Warn("unauthorized user command", "user_id", msg.From.ID)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Unauthorized.",
		})
		return
	}

	parts := strings.SplitN(msg.Text, " ", 2)
	cmd := strings.TrimPrefix(parts[0], "/")
	// Strip bot username suffix (e.g., /start@mybotname)
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}

	response, err := h.bridge.HandleCommand(ctx, msg.Chat.ID, cmd, args)
	if err != nil {
		slog.Error("bridge handle command failed", "error", err, "cmd", cmd)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: msg.Chat.ID,
			Text:   "Error: " + err.Error(),
		})
		return
	}

	h.sendChunked(ctx, b, msg.Chat.ID, response)
}

// sendChunked splits long messages at paragraph boundaries and sends them sequentially.
func (h *Handler) sendChunked(ctx context.Context, b *bot.Bot, chatID int64, text string) {
	if text == "" {
		text = "(empty response)"
	}

	chunks := splitMessage(text, maxMessageLength)
	for _, chunk := range chunks {
		_, err := b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID: chatID,
			Text:   chunk,
		})
		if err != nil {
			slog.Error("failed to send message", "error", err, "chat_id", chatID)
			return
		}
	}
}

// splitMessage splits text into chunks of at most maxLen characters,
// preferring to split at paragraph boundaries (\n\n), then line boundaries (\n).
func splitMessage(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Try to split at paragraph boundary
		chunk := text[:maxLen]
		splitIdx := strings.LastIndex(chunk, "\n\n")
		if splitIdx == -1 {
			// Try line boundary
			splitIdx = strings.LastIndex(chunk, "\n")
		}
		if splitIdx == -1 {
			// Try space
			splitIdx = strings.LastIndex(chunk, " ")
		}
		if splitIdx == -1 {
			// Hard split
			splitIdx = maxLen - 1
		}

		chunks = append(chunks, text[:splitIdx+1])
		text = text[splitIdx+1:]
	}
	return chunks
}

func (h *Handler) sendTypingPeriodically(ctx context.Context, b *bot.Bot, chatID int64) {
	// Send immediately
	b.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID: chatID,
		Action: models.ChatActionTyping,
	})

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.SendChatAction(ctx, &bot.SendChatActionParams{
				ChatID: chatID,
				Action: models.ChatActionTyping,
			})
		}
	}
}
