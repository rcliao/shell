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

// HeadingPrefixes controls optional text prefixes prepended to each heading
// level when rendered in Telegram MarkdownV2. Index 0 = H1, 1 = H2, 2 = H3+.
// Empty strings (the default) mean no prefix — headings are distinguished by
// formatting alone (bold+underline, bold, italic).
var HeadingPrefixes = [3]string{"", "", ""}

const longRunningThreshold = 15 * time.Second // time before switching reaction from 👀 to ⏳

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

		// 6. Headings: # at start of line → formatting-based visual hierarchy
		if text[i] == '#' && (i == 0 || text[i-1] == '\n') {
			// Count heading level and skip # characters
			j := i
			level := 0
			for j < n && text[j] == '#' {
				j++
				level++
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
			escaped := escapeMarkdownV2Text(heading)
			// Determine prefix index: 0=H1, 1=H2, 2=H3+.
			pi := level - 1
			if pi > 2 {
				pi = 2
			}
			prefix := ""
			if HeadingPrefixes[pi] != "" {
				prefix = escapeMarkdownV2Text(HeadingPrefixes[pi])
			}
			switch {
			case level == 1:
				// H1: bold + underline (strongest emphasis)
				result.WriteString("*__")
				result.WriteString(prefix)
				result.WriteString(escaped)
				result.WriteString("__*")
			case level == 2:
				// H2: bold
				result.WriteString("*")
				result.WriteString(prefix)
				result.WriteString(escaped)
				result.WriteString("*")
			default:
				// H3+: italic
				result.WriteString("_")
				result.WriteString(prefix)
				result.WriteString(escaped)
				result.WriteString("_")
			}
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

		// 9. Blockquote: "> text" at start of a line
		if text[i] == '>' && isLineStart(text, i) {
			// Skip optional space after >
			j := i + 1
			if j < n && text[j] == ' ' {
				j++
			}
			lineEnd := strings.Index(text[j:], "\n")
			var content string
			if lineEnd == -1 {
				content = text[j:]
				lineEnd = n - j
			} else {
				content = text[j : j+lineEnd]
			}
			result.WriteString(">")
			result.WriteString(formatForMarkdownV2(content))
			i = j + lineEnd
			if i < n && text[i] == '\n' {
				result.WriteByte('\n')
				i++
			}
			continue
		}

		// 10. Strikethrough: ~~text~~ → ~text~ (Telegram MarkdownV2)
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

// closeOpenMarkdown detects unclosed Markdown formatting in text (as occurs
// mid-stream) and appends closing markers so that formatForMarkdownV2 can
// produce valid, nicely formatted MarkdownV2 instead of escaping unclosed
// markers as literal characters.
func closeOpenMarkdown(text string) string {
	n := len(text)
	if n == 0 {
		return text
	}

	i := 0
	inFencedCode := false
	inInlineCode := false

	type marker struct {
		token string
		pos   int
	}
	var open []marker

	for i < n {
		// Fenced code blocks: ```
		if !inInlineCode && i+2 < n && text[i] == '`' && text[i+1] == '`' && text[i+2] == '`' {
			if inFencedCode {
				inFencedCode = false
				i += 3
				continue
			}
			inFencedCode = true
			i += 3
			// Skip past language tag / rest of opening line
			for i < n && text[i] != '\n' {
				i++
			}
			continue
		}

		// Inside fenced code block, just advance
		if inFencedCode {
			i++
			continue
		}

		// Inline code: `
		if text[i] == '`' {
			// Check for a closing backtick on the same line
			if i+1 < n {
				end := strings.Index(text[i+1:], "`")
				if end != -1 && !strings.Contains(text[i+1:i+1+end], "\n") {
					// Complete inline code, skip past it
					i = i + 1 + end + 1
					continue
				}
			}
			// Unclosed inline code
			inInlineCode = true
			i++
			continue
		}

		// Inside inline code, just advance
		if inInlineCode {
			i++
			continue
		}

		// Bold+Italic: ***
		if i+2 < n && text[i] == '*' && text[i+1] == '*' && text[i+2] == '*' {
			end := strings.Index(text[i+3:], "***")
			if end != -1 && end > 0 {
				i = i + 3 + end + 3
				continue
			}
			open = append(open, marker{"***", i})
			i += 3
			continue
		}

		// Bold: **
		if i+1 < n && text[i] == '*' && text[i+1] == '*' {
			end := strings.Index(text[i+2:], "**")
			if end != -1 && end > 0 {
				i = i + 2 + end + 2
				continue
			}
			open = append(open, marker{"**", i})
			i += 2
			continue
		}

		// Italic: * (single, not adjacent to another *)
		if text[i] == '*' && (i == 0 || text[i-1] != '*') && (i+1 >= n || text[i+1] != '*') {
			end := strings.Index(text[i+1:], "*")
			if end != -1 && end > 0 {
				closePos := i + 1 + end
				if closePos+1 >= n || text[closePos+1] != '*' {
					i = closePos + 1
					continue
				}
			}
			open = append(open, marker{"*", i})
			i++
			continue
		}

		// Strikethrough: ~~
		if i+1 < n && text[i] == '~' && text[i+1] == '~' {
			end := strings.Index(text[i+2:], "~~")
			if end != -1 && end > 0 {
				i = i + 2 + end + 2
				continue
			}
			open = append(open, marker{"~~", i})
			i += 2
			continue
		}

		i++
	}

	// Nothing to close
	if !inFencedCode && !inInlineCode && len(open) == 0 {
		return text
	}

	var suffix strings.Builder

	if inFencedCode {
		suffix.WriteString("\n```")
	} else {
		if inInlineCode {
			suffix.WriteByte('`')
		}
		// Close formatting markers in reverse order (innermost first).
		// Only close if there is actual content after the opening marker;
		// a bare marker with nothing after it (e.g. trailing "**") is left
		// for the formatter to escape normally.
		for j := len(open) - 1; j >= 0; j-- {
			m := open[j]
			if strings.TrimSpace(text[m.pos+len(m.token):]) != "" {
				suffix.WriteString(m.token)
			}
		}
	}

	if suffix.Len() == 0 {
		return text
	}

	return text + suffix.String()
}

// formatForTelegram converts Markdown text to Telegram MarkdownV2 format,
// ensuring the result fits within maxLen bytes. It closes any unclosed
// Markdown formatting before conversion (important for streaming content).
// If the formatted text exceeds maxLen, it retries with progressively
// shorter raw text (showing the tail). Returns the formatted string and
// true if MarkdownV2 was applied, or the truncated raw text and false
// as a fallback.
func formatForTelegram(text string, maxLen int) (string, bool) {
	formatted := formatForMarkdownV2(closeOpenMarkdown(text))
	if len(formatted) <= maxLen {
		return formatted, true
	}

	// Formatted text exceeds the limit — retry with shorter raw text.
	// MarkdownV2 escaping adds at most one backslash per character (≤2×),
	// so halving the raw text guarantees the formatted result fits.
	for _, limit := range []int{maxLen * 2 / 3, maxLen / 2, maxLen / 3} {
		if limit >= len(text) {
			continue
		}
		truncated := "..." + text[len(text)-limit:]
		formatted = formatForMarkdownV2(closeOpenMarkdown(truncated))
		if len(formatted) <= maxLen {
			return formatted, true
		}
	}

	// Could not fit even at half length. Return truncated raw text.
	if len(text) > maxLen-3 {
		return "..." + text[len(text)-maxLen+3:], false
	}
	return text, false
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

// looksLikeClarification checks if a response appears to be asking the user
// for clarification (i.e. it ends with a question mark).
func looksLikeClarification(response string) bool {
	trimmed := strings.TrimSpace(response)
	return strings.HasSuffix(trimmed, "?")
}

func (h *Handler) HandleReaction(ctx context.Context, b *bot.Bot, reaction *models.MessageReactionUpdated) {
	slog.Info("received message reaction",
		"chat_id", reaction.Chat.ID,
		"message_id", reaction.MessageID,
		"new_reaction", reaction.NewReaction,
		"old_reaction", reaction.OldReaction,
	)

	// Auth check: only process reactions from allowed users.
	if reaction.User != nil && !h.auth.IsAllowed(reaction.User.ID) {
		slog.Warn("unauthorized reaction", "user_id", reaction.User.ID)
		return
	}

	// Extract the first emoji from new reactions.
	if len(reaction.NewReaction) == 0 {
		return
	}
	emoji := ""
	for _, r := range reaction.NewReaction {
		if r.ReactionTypeEmoji != nil {
			emoji = r.ReactionTypeEmoji.Emoji
			break
		}
	}
	if emoji == "" {
		return
	}

	chatID := reaction.Chat.ID
	response, err := h.bridge.HandleReaction(ctx, chatID, emoji)
	if err != nil {
		slog.Error("bridge handle reaction failed", "error", err, "chat_id", chatID, "emoji", emoji)
		return
	}
	if response == "" {
		return
	}

	h.sendChunked(ctx, b, chatID, response)
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

	// Build sender display name for Claude to identify who is speaking.
	senderName := msg.From.FirstName
	if senderName == "" {
		senderName = msg.From.Username
	}

	// React with 👀 to acknowledge receipt.
	setReaction(ctx, b, msg.Chat.ID, msg.ID, "👀")

	// Switch to ⏳ if processing takes a while.
	longRunning := time.AfterFunc(longRunningThreshold, func() {
		setReaction(ctx, b, msg.Chat.ID, msg.ID, "⏳")
	})
	defer longRunning.Stop()

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
	markdownFailed := false   // set when MarkdownV2 is rejected during streaming
	lastSentContent := ""     // raw text of last successful streaming edit
	lastUsedMarkdown := false // whether last streaming edit used MarkdownV2

	onUpdate := func(chunk string) {
		mu.Lock()
		accumulated.WriteString(chunk)
		current := accumulated.String()
		now := time.Now()
		shouldEdit := now.Sub(lastEdit) >= streamEditInterval
		if shouldEdit {
			lastEdit = now
		}
		failed := markdownFailed
		mu.Unlock()

		if !shouldEdit {
			return
		}

		if !failed {
			// Format for MarkdownV2, truncating from the front if the
			// formatted result would exceed Telegram's message-length limit.
			formatted, ok := formatForTelegram(current, maxMessageLength)
			if ok {
				_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    msg.Chat.ID,
					MessageID: msgID,
					Text:      formatted,
					ParseMode: models.ParseModeMarkdown,
				})
				if editErr == nil {
					mu.Lock()
					lastSentContent = current
					lastUsedMarkdown = true
					mu.Unlock()
					return
				}
				slog.Debug("streaming markdown edit failed, disabling for remaining edits", "error", editErr)
				mu.Lock()
				markdownFailed = true
				mu.Unlock()
			}
		}

		// Fallback: send without formatting if MarkdownV2 was rejected or unavailable.
		plain := current
		if len(plain) > maxMessageLength-3 {
			plain = "..." + plain[len(plain)-maxMessageLength+3:]
		}
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      plain,
		})
		if editErr != nil {
			slog.Debug("failed to edit streaming message", "error", editErr)
		} else {
			mu.Lock()
			lastSentContent = current
			lastUsedMarkdown = false
			mu.Unlock()
		}
	}

	response, err := h.bridge.HandleMessageStreaming(ctx, msg.Chat.ID, text, senderName, onUpdate)

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

	// Final response: skip if the last streaming edit already displayed
	// this content with MarkdownV2 formatting. Otherwise edit in place
	// if it fits, or delete and send chunked.
	formatted := formatForMarkdownV2(response)

	mu.Lock()
	alreadySent := lastUsedMarkdown && lastSentContent == response
	mu.Unlock()

	if alreadySent && len(formatted) <= maxMessageLength {
		// Streaming already displayed the final formatted content.
	} else if len(formatted) <= maxMessageLength {
		_, editErr := b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      formatted,
			ParseMode: models.ParseModeMarkdown,
		})
		if editErr != nil {
			// Fallback: try without markdown formatting
			setReaction(ctx, b, msg.Chat.ID, msg.ID, "🔄")
			b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    msg.Chat.ID,
				MessageID: msgID,
				Text:      response,
			})
		}
	} else if len(response) <= maxMessageLength {
		// Formatted text exceeds the limit but raw text fits — send unformatted.
		b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
			Text:      response,
		})
	} else {
		// Delete placeholder and send chunked formatted response.
		b.DeleteMessage(ctx, &bot.DeleteMessageParams{
			ChatID:    msg.Chat.ID,
			MessageID: msgID,
		})
		h.sendChunked(ctx, b, msg.Chat.ID, response)
	}

	// Pick a finishing reaction: 🤔 when Claude is asking for clarification, ✅ otherwise.
	finalEmoji := "✅"
	if looksLikeClarification(response) {
		finalEmoji = "🤔"
	}
	setReaction(ctx, b, msg.Chat.ID, msg.ID, finalEmoji)
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

// splitMessage splits text into chunks such that each chunk, after being
// formatted with formatForMarkdownV2, fits within maxLen bytes. It prefers
// to split at paragraph boundaries (\n\n), then line boundaries (\n).
func splitMessage(text string, maxLen int) []string {
	if len(formatForMarkdownV2(text)) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(formatForMarkdownV2(text)) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// Find the largest raw prefix whose formatted length fits in maxLen.
		// Start optimistically at maxLen raw chars, then shrink proportionally.
		end := maxLen
		if end > len(text) {
			end = len(text)
		}
		for end > 1 {
			fmtLen := len(formatForMarkdownV2(text[:end]))
			if fmtLen <= maxLen {
				break
			}
			// Shrink proportionally with guaranteed progress.
			newEnd := end * maxLen / fmtLen
			if newEnd >= end {
				newEnd = end - 1
			}
			if newEnd < 1 {
				newEnd = 1
			}
			end = newEnd
		}

		// Try to split at a nice boundary within [0, end].
		chunk := text[:end]
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
			splitIdx = end - 1
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
