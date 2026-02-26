package telegram

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rcliao/teeny-relay/internal/bridge"
)

const streamEditInterval = time.Second // minimum interval between Telegram message edits

const maxMessageLength = 4096

// escapeMarkdownV2Text escapes special characters for Telegram MarkdownV2
// in plain text that should not be interpreted as formatting.
func escapeMarkdownV2Text(text string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`_`, `\_`,
		`*`, `\*`,
		`[`, `\[`,
		`]`, `\]`,
		`(`, `\(`,
		`)`, `\)`,
		`~`, `\~`,
		"`", "\\`",
		`>`, `\>`,
		`#`, `\#`,
		`+`, `\+`,
		`-`, `\-`,
		`=`, `\=`,
		`|`, `\|`,
		`{`, `\{`,
		`}`, `\}`,
		`.`, `\.`,
		`!`, `\!`,
	)
	return replacer.Replace(text)
}

// escapeCodeContent escapes only \ and ` inside code blocks/inline code
// per Telegram MarkdownV2 rules.
func escapeCodeContent(text string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
	)
	return replacer.Replace(text)
}

// escapeMarkdownV2URL escapes only \ and ) inside the URL part of an inline
// link, per Telegram MarkdownV2 rules.
func escapeMarkdownV2URL(url string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`)`, `\)`,
	)
	return replacer.Replace(url)
}

// isLineStart reports whether position i in text is at the effective start
// of a line: either at position 0, right after a newline, or preceded only
// by whitespace since the last newline (or start of string).
func isLineStart(text string, i int) bool {
	if i == 0 || text[i-1] == '\n' {
		return true
	}
	for j := i - 1; j >= 0; j-- {
		if text[j] == '\n' {
			return true
		}
		if text[j] != ' ' && text[j] != '\t' {
			return false
		}
	}
	return true
}

// mdLinkRe matches standard Markdown links: [text](url)
var mdLinkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)

// formatForMarkdownV2 converts standard Markdown (as output by Claude) to
// Telegram MarkdownV2 format with selective escaping. It preserves bold,
// italic, code blocks, inline code, and links.
func formatForMarkdownV2(text string) string {
	var result strings.Builder
	i := 0
	n := len(text)

	for i < n {
		// 1. Fenced code blocks: ```lang\ncode```
		if i+2 < n && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			end := strings.Index(text[i+3:], "```")
			if end != -1 {
				content := text[i+3 : i+3+end]
				result.WriteString("```")
				result.WriteString(escapeCodeContent(content))
				result.WriteString("```")
				i = i + 3 + end + 3
				continue
			}
		}

		// 2. Inline code: `code`
		if text[i] == '`' {
			end := strings.Index(text[i+1:], "`")
			if end != -1 && !strings.Contains(text[i+1:i+1+end], "\n") {
				content := text[i+1 : i+1+end]
				result.WriteByte('`')
				result.WriteString(escapeCodeContent(content))
				result.WriteByte('`')
				i = i + 1 + end + 1
				continue
			}
		}

		// 3a. Bold+Italic: ***text*** → *_text_* (Telegram MarkdownV2)
		if i+2 < n && text[i] == '*' && text[i+1] == '*' && text[i+2] == '*' {
			end := strings.Index(text[i+3:], "***")
			if end != -1 && end > 0 {
				inner := text[i+3 : i+3+end]
				result.WriteString("*_")
				result.WriteString(escapeMarkdownV2Text(inner))
				result.WriteString("_*")
				i = i + 3 + end + 3
				continue
			}
		}

		// 3b. Bold: **text** → *text* (Telegram MarkdownV2 bold)
		if i+1 < n && text[i] == '*' && text[i+1] == '*' {
			end := strings.Index(text[i+2:], "**")
			if end != -1 && end > 0 {
				inner := text[i+2 : i+2+end]
				result.WriteString("*")
				result.WriteString(escapeMarkdownV2Text(inner))
				result.WriteString("*")
				i = i + 2 + end + 2
				continue
			}
		}

		// 4. Italic: *text* → _text_ (Telegram MarkdownV2 italic)
		// Only match single * not preceded by another *
		if text[i] == '*' && (i == 0 || text[i-1] != '*') && (i+1 >= n || text[i+1] != '*') {
			end := strings.Index(text[i+1:], "*")
			if end != -1 && end > 0 {
				// Ensure the closing * is not part of **
				closePos := i + 1 + end
				if closePos+1 >= n || text[closePos+1] != '*' {
					inner := text[i+1 : i+1+end]
					result.WriteString("_")
					result.WriteString(escapeMarkdownV2Text(inner))
					result.WriteString("_")
					i = closePos + 1
					continue
				}
			}
		}

		// 5. Links: [text](url)
		if text[i] == '[' {
			remaining := text[i:]
			loc := mdLinkRe.FindStringIndex(remaining)
			if loc != nil && loc[0] == 0 {
				matches := mdLinkRe.FindStringSubmatch(remaining)
				linkText := matches[1]
				linkURL := matches[2]
				result.WriteString("[")
				result.WriteString(escapeMarkdownV2Text(linkText))
				result.WriteString("](")
				result.WriteString(escapeMarkdownV2URL(linkURL))
				result.WriteString(")")
				i += loc[1]
				continue
			}
		}

		// 6. Headings: # at start of line → bold text
		if text[i] == '#' && (i == 0 || text[i-1] == '\n') {
			// Count heading level and skip # characters
			j := i
			for j < n && text[j] == '#' {
				j++
			}
			// Skip space after #
			if j < n && text[j] == ' ' {
				j++
			}
			// Find end of line
			lineEnd := strings.Index(text[j:], "\n")
			var heading string
			if lineEnd == -1 {
				heading = text[j:]
				lineEnd = n - j
			} else {
				heading = text[j : j+lineEnd]
			}
			result.WriteString("*")
			result.WriteString(escapeMarkdownV2Text(heading))
			result.WriteString("*")
			i = j + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 7. Bullet list item: "- text" at start of a line (with optional leading whitespace)
		if text[i] == '-' && i+1 < n && text[i+1] == ' ' && isLineStart(text, i) {
			lineEnd := strings.Index(text[i+2:], "\n")
			var content string
			if lineEnd == -1 {
				content = text[i+2:]
				lineEnd = n - (i + 2)
			} else {
				content = text[i+2 : i+2+lineEnd]
			}
			result.WriteString("\\- ")
			result.WriteString(escapeMarkdownV2Text(content))
			i = i + 2 + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 8. Numbered list item: "1. text" at start of a line (with optional leading whitespace)
		if text[i] >= '0' && text[i] <= '9' && isLineStart(text, i) {
			j := i + 1
			for j < n && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			if j+1 < n && text[j] == '.' && text[j+1] == ' ' {
				lineEnd := strings.Index(text[j+2:], "\n")
				var content string
				if lineEnd == -1 {
					content = text[j+2:]
					lineEnd = n - (j + 2)
				} else {
					content = text[j+2 : j+2+lineEnd]
				}
				result.WriteString(text[i:j])
				result.WriteString("\\. ")
				result.WriteString(escapeMarkdownV2Text(content))
				i = j + 2 + lineEnd
				if i < n && text[i] == '\n' {
					result.WriteByte('\n')
					i++
				}
				continue
			}
		}

		// 9. Strikethrough: ~~text~~ → ~text~ (Telegram MarkdownV2)
		if i+1 < n && text[i] == '~' && text[i+1] == '~' {
			end := strings.Index(text[i+2:], "~~")
			if end != -1 && end > 0 {
				inner := text[i+2 : i+2+end]
				result.WriteString("~")
				result.WriteString(escapeMarkdownV2Text(inner))
				result.WriteString("~")
				i = i + 2 + end + 2
				continue
			}
		}

		// Plain text character — escape for MarkdownV2
		c := text[i]
		switch c {
		case '\\', '_', '*', '[', ']', '(', ')', '~', '`', '>', '#', '+', '-', '=', '|', '{', '}', '.', '!':
			result.WriteByte('\\')
			result.WriteByte(c)
		default:
			result.WriteByte(c)
		}
		i++
	}

	return result.String()
}

type Handler struct {
	auth   *Auth
	bridge *bridge.Bridge
}

func NewHandler(auth *Auth, br *bridge.Bridge) *Handler {
	return &Handler{auth: auth, bridge: br}
}

// setReaction sets an emoji reaction on a message, replacing any previous reaction.
func setReaction(ctx context.Context, b *bot.Bot, chatID any, messageID int, emoji string) {
	_, err := b.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []models.ReactionType{
			{
				Type:              models.ReactionTypeTypeEmoji,
				ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
			},
		},
	})
	if err != nil {
		slog.Debug("failed to set reaction", "error", err, "emoji", emoji)
	}
}

func (h *Handler) HandleMessage(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil {
		return
	}
	if !h.auth.IsAllowed(msg.From.ID) {
		slog.Warn("unauthorized user", "user_id", msg.From.ID, "username", msg.From.Username)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    msg.Chat.ID,
			Text:      escapeMarkdownV2Text("Unauthorized. Your user ID is not in the allowlist."),
			ParseMode: models.ParseModeMarkdown,
		})
		return
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}

	// React with 👀 to acknowledge receipt.
	setReaction(ctx, b, msg.Chat.ID, msg.ID, "👀")

	// Send an initial placeholder message that we'll edit with streaming updates.
	placeholder, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   escapeMarkdownV2Text("Thinking..."),
		ParseMode: models.ParseModeMarkdown,
	})
	if err != nil {
		slog.Error("failed to send placeholder", "error", err, "chat_id", msg.Chat.ID)
		return
	}
	msgID := placeholder.ID

	// Set up streaming state: accumulate text and throttle edits.
	var mu sync.Mutex
	var accumulated strings.Builder
	lastEdit := time.Time{}

	onUpdate := func(chunk string) {
		mu.Lock()
		accumulated.WriteString(chunk)
		current := accumulated.String()
		now := time.Now()
		shouldEdit := now.Sub(lastEdit) >= streamEditInterval
		if shouldEdit {
			lastEdit = now
		}
		mu.Unlock()

		if !shouldEdit {
			return
		}

		// Truncate for Telegram's 4096 limit, showing the tail if too long.
		display := current
		if len(display) > maxMessageLength-10 {
			display = "..." + display[len(display)-maxMessageLength+10:]
		}

		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      display,
		})
		if editErr != nil {
			slog.Debug("failed to edit streaming message", "error", editErr)
		}
	}

	response, err := h.bridge.HandleMessageStreaming(ctx, msg.Chat.ID, text, onUpdate)

	if err != nil {
		slog.Error("bridge handle message failed", "error", err, "chat_id", msg.Chat.ID)
		setReaction(ctx, b, msg.Chat.ID, msg.ID, "❌")
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      escapeMarkdownV2Text("Error: " + err.Error()),
			ParseMode: models.ParseModeMarkdown,
		})
		return
	}

	if response == "" {
		response = "(empty response)"
	}

	// Final response: edit in place if it fits, otherwise delete and send chunked.
	if len(response) <= maxMessageLength {
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      formatForMarkdownV2(response),
			ParseMode: models.ParseModeMarkdown,
		})
		if editErr != nil {
			// Fallback: try without markdown formatting
			b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    msg.Chat.ID,
				MessageID: msgID,
				Text:      response,
			})
		}
	} else {
		// Delete placeholder and send chunked formatted response.
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
		})
		h.sendChunked(ctx, b, msg.Chat.ID, response)
	}

	setReaction(ctx, b, msg.Chat.ID, msg.ID, "✅")
}

func (h *Handler) HandleCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	if msg.From == nil {
		return
	}
	if !h.auth.IsAllowed(msg.From.ID) {
		slog.Warn("unauthorized user command", "user_id", msg.From.ID)
		b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    msg.Chat.ID,
			Text:      escapeMarkdownV2Text("Unauthorized."),
			ParseMode: models.ParseModeMarkdown,
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
			ChatID:    msg.Chat.ID,
			Text:      escapeMarkdownV2Text("Error: " + err.Error()),
			ParseMode: models.ParseModeMarkdown,
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
			ChatID:    chatID,
			Text:      formatForMarkdownV2(chunk),
			ParseMode: models.ParseModeMarkdown,
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
