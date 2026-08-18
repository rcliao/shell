package telegram

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rcliao/shell/internal/bridge"
)

type Bot struct {
	bot     *bot.Bot
	auth    *Auth
	bridge  *bridge.Bridge
	handler *Handler
	// dedup, when set, is consulted before every proactive SendText; returning
	// true suppresses the send (V2-H3 outbound dedup ledger).
	dedup func(chatID, threadID int64, text string) bool
}

// SetOutboundDedup installs the proactive-send dedup check. SendText carries
// only proactive traffic (scheduler notify, relay, a2a, prompt-schedule
// results) — conversational replies go through the message-edit path — so
// this is the single chokepoint for the duplicate-reminder guard.
func (b *Bot) SetOutboundDedup(check func(chatID, threadID int64, text string) bool) {
	b.dedup = check
}

func NewBot(token string, auth *Auth, br *bridge.Bridge, agentCfg AgentConfig) (*Bot, error) {
	if token == "" {
		return nil, fmt.Errorf("telegram bot token is empty")
	}

	b := &Bot{
		auth:   auth,
		bridge: br,
	}
	b.handler = NewHandler(auth, br, agentCfg)

	opts := []bot.Option{
		bot.WithDefaultHandler(b.defaultHandler),
		bot.WithAllowedUpdates(bot.AllowedUpdates{
			models.AllowedUpdateMessage,
			models.AllowedUpdateMessageReaction,
		}),
	}

	tgBot, err := bot.New(token, opts...)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	// Register command handlers
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/start", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/new", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/status", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/help", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/reactions", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/remember", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/forget", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/memories", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/plan", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/planstatus", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/planstop", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/planskip", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/planretry", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/schedule", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/heartbeat", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/personality", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/skills", bot.MatchTypePrefix, b.commandHandler)
	tgBot.RegisterHandler(bot.HandlerTypeMessageText, "/projects", bot.MatchTypePrefix, b.commandHandler)
	// Register handler for photo messages.
	tgBot.RegisterHandlerMatchFunc(
		func(update *models.Update) bool {
			return update.Message != nil && len(update.Message.Photo) > 0
		},
		b.photoHandler,
	)

	// Register handler for sticker messages.
	tgBot.RegisterHandlerMatchFunc(
		func(update *models.Update) bool {
			return update.Message != nil && update.Message.Sticker != nil
		},
		b.stickerHandler,
	)

	// Register handler for PDF documents.
	tgBot.RegisterHandlerMatchFunc(
		func(update *models.Update) bool {
			return update.Message != nil && IsPDFDocument(update.Message.Document)
		},
		b.pdfHandler,
	)

	// Register handler for documents with image MIME types (uncompressed photos).
	tgBot.RegisterHandlerMatchFunc(
		func(update *models.Update) bool {
			return update.Message != nil && IsImageDocument(update.Message.Document)
		},
		b.defaultHandler,
	)

	// Register handler for incoming emoji reactions.
	tgBot.RegisterHandlerMatchFunc(
		func(update *models.Update) bool { return update.MessageReaction != nil },
		b.reactionHandler,
	)

	b.bot = tgBot
	return b, nil
}

// Start begins long polling. Blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	slog.Info("telegram bot starting long poll")
	b.bot.Start(ctx)
}

// SendText sends a message to a chat/topic, splitting at paragraph boundaries if needed.
// Used for async notifications like plan progress. threadID is the Telegram
// forum topic ID (0 = main chat / no topic).
func (b *Bot) SendText(chatID, threadID int64, text string) {
	// Errors are already logged inside; SendText keeps its fire-and-forget
	// contract.
	_ = b.SendTextButtons(chatID, threadID, text, nil)
}

// SendTextButtons is SendText plus inline URL buttons. When the text splits
// into multiple messages the buttons ride the LAST one, so they sit under the
// end of the notification. An empty buttons slice behaves exactly like
// SendText; unlike SendText this returns the (last) send error so callers can
// tell whether a delivery landed.
func (b *Bot) SendTextButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) error {
	if b.dedup != nil && b.dedup(chatID, threadID, text) {
		return nil
	}
	ctx := context.Background()
	chunks := splitMessage(text, maxMessageLength)
	var lastErr error
	for i, chunk := range chunks {
		var markup models.ReplyMarkup
		if i == len(chunks)-1 {
			markup = linkButtonMarkup(buttons)
		}
		_, err := b.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: int(threadID),
			Text:            formatForMarkdownV2(chunk),
			ParseMode:       models.ParseModeMarkdown,
			ReplyMarkup:     markup,
		})
		if err != nil {
			slog.Warn("MarkdownV2 send failed, retrying as plain text", "error", err, "chat_id", chatID, "thread_id", threadID)
			_, err = b.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          chatID,
				MessageThreadID: int(threadID),
				Text:            chunk,
				ReplyMarkup:     markup,
			})
			if err != nil {
				slog.Error("failed to send notification", "error", err, "chat_id", chatID, "thread_id", threadID)
				lastErr = err
			}
		}
	}
	return lastErr
}

// SendPhoto sends an image to a chat/topic as a Telegram photo message.
func (b *Bot) SendPhoto(chatID, threadID int64, imageData []byte, caption string) {
	ctx := context.Background()
	_, err := b.bot.SendPhoto(ctx, &bot.SendPhotoParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Photo: &models.InputFileUpload{
			Filename: "image.png",
			Data:     bytes.NewReader(imageData),
		},
		Caption: caption,
	})
	if err != nil {
		slog.Error("failed to send photo", "error", err, "chat_id", chatID, "thread_id", threadID)
	}
}

// SendVideo sends a video to a chat/topic as a Telegram video message.
func (b *Bot) SendVideo(chatID, threadID int64, videoData []byte, caption string) {
	ctx := context.Background()
	_, err := b.bot.SendVideo(ctx, &bot.SendVideoParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Video: &models.InputFileUpload{
			Filename: "video.mp4",
			Data:     bytes.NewReader(videoData),
		},
		Caption: caption,
	})
	if err != nil {
		slog.Error("failed to send video", "error", err, "chat_id", chatID, "thread_id", threadID)
	}
}

// SendDocument sends a file (by path) to a chat/topic as a Telegram document.
// Flood-retried like the media sends: document delivery is a receipt surface
// and must not be silently dropped by a 429.
func (b *Bot) SendDocument(chatID, threadID int64, path, caption string) error {
	f, err := os.Open(path)
	if err != nil {
		slog.Error("failed to open document", "error", err, "path", path)
		return err
	}
	defer f.Close()
	ctx := context.Background()
	err = withFloodRetry(ctx, func() error {
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return serr
		}
		_, serr := b.bot.SendDocument(ctx, &bot.SendDocumentParams{
			ChatID:          chatID,
			MessageThreadID: int(threadID),
			Document: &models.InputFileUpload{
				Filename: filepath.Base(path),
				Data:     f,
			},
			Caption: caption,
		})
		return serr
	})
	if err != nil {
		slog.Error("failed to send document", "error", err, "chat_id", chatID, "thread_id", threadID, "path", path)
	}
	return err
}

// SendMessageID sends a single text message and returns its message ID, so
// callers can pin or edit it later (SendText returns nothing). MarkdownV2 is
// tried first with a plain-text fallback, same as SendText; the text is NOT
// chunk-split — a message whose id matters (the pinned 📋 Projects list) must
// stay one message, so keep it short.
func (b *Bot) SendMessageID(chatID, threadID int64, text string) (int, error) {
	return b.SendMessageIDButtons(chatID, threadID, text, nil)
}

// SendMessageIDButtons is SendMessageID plus inline URL buttons. An empty
// buttons slice behaves exactly like SendMessageID.
func (b *Bot) SendMessageIDButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) (int, error) {
	ctx := context.Background()
	markup := linkButtonMarkup(buttons)
	msg, err := b.bot.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Text:            formatForMarkdownV2(text),
		ParseMode:       models.ParseModeMarkdown,
		ReplyMarkup:     markup,
	})
	if err != nil {
		slog.Warn("MarkdownV2 send failed, retrying as plain text", "error", err, "chat_id", chatID, "thread_id", threadID)
		msg, err = b.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          chatID,
			MessageThreadID: int(threadID),
			Text:            text,
			ReplyMarkup:     markup,
		})
	}
	if err != nil {
		slog.Error("failed to send message", "error", err, "chat_id", chatID, "thread_id", threadID)
		return 0, err
	}
	return msg.ID, nil
}

// EditMessage replaces the text of a previously sent message in place,
// through the flood-retry path — a pinned-list update that lands during a
// flood window must not be silently dropped. An "is not modified" response
// (identical content) is treated as success.
func (b *Bot) EditMessage(chatID int64, messageID int, text string) error {
	return b.EditMessageButtons(chatID, messageID, text, nil)
}

// EditMessageButtons is EditMessage plus inline URL buttons; the buttons
// replace whatever keyboard the message previously carried (an empty slice
// clears it — Telegram edits drop an omitted reply_markup).
func (b *Bot) EditMessageButtons(chatID int64, messageID int, text string, buttons []bridge.LinkButton) error {
	ctx := context.Background()
	markup := linkButtonMarkup(buttons)
	err := editFinal(ctx, b.bot, &bot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        formatForMarkdownV2(text),
		ParseMode:   models.ParseModeMarkdown,
		ReplyMarkup: markup,
	})
	if err != nil && !isNotModified(err) {
		// Same fallback ladder as sends: the MarkdownV2 escape can be rejected
		// for content reasons, and losing the edit entirely is worse than
		// losing the markup.
		err = editFinal(ctx, b.bot, &bot.EditMessageTextParams{
			ChatID:      chatID,
			MessageID:   messageID,
			Text:        text,
			ReplyMarkup: markup,
		})
	}
	if err != nil && !isNotModified(err) {
		slog.Error("failed to edit message", "error", err, "chat_id", chatID, "message_id", messageID)
		return err
	}
	return nil
}

// linkButtonMarkup renders LinkButtons as an inline keyboard, one button per
// row. Empty input returns nil, which omits reply_markup entirely.
func linkButtonMarkup(buttons []bridge.LinkButton) models.ReplyMarkup {
	if len(buttons) == 0 {
		return nil
	}
	rows := make([][]models.InlineKeyboardButton, 0, len(buttons))
	for _, btn := range buttons {
		rows = append(rows, []models.InlineKeyboardButton{{Text: btn.Label, URL: btn.URL}})
	}
	return &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// PinMessage pins a message in a chat; silent suppresses the notification.
func (b *Bot) PinMessage(chatID int64, messageID int, silent bool) error {
	ctx := context.Background()
	err := withFloodRetry(ctx, func() error {
		_, serr := b.bot.PinChatMessage(ctx, &bot.PinChatMessageParams{
			ChatID:              chatID,
			MessageID:           messageID,
			DisableNotification: silent,
		})
		return serr
	})
	if err != nil {
		slog.Error("failed to pin message", "error", err, "chat_id", chatID, "message_id", messageID)
	}
	return err
}

// UnpinMessage unpins a previously pinned message.
func (b *Bot) UnpinMessage(chatID int64, messageID int) error {
	ctx := context.Background()
	err := withFloodRetry(ctx, func() error {
		_, serr := b.bot.UnpinChatMessage(ctx, &bot.UnpinChatMessageParams{
			ChatID:    chatID,
			MessageID: messageID,
		})
		return serr
	})
	if err != nil {
		slog.Error("failed to unpin message", "error", err, "chat_id", chatID, "message_id", messageID)
	}
	return err
}

// SendChatAction sends a chat action (e.g. "upload_photo", "typing") to a chat/topic.
func (b *Bot) SendChatAction(chatID, threadID int64, action string) {
	ctx := context.Background()
	b.bot.SendChatAction(ctx, &bot.SendChatActionParams{
		ChatID:          chatID,
		MessageThreadID: int(threadID),
		Action:          models.ChatAction(action),
	})
}

// turnContext detaches a handler from the long-poller's lifetime.
//
// Drain stops the poller by cancelling the context it was started with, so a
// deploy stops fetching new updates. But the library hands that SAME context to
// every handler, so cancelling it also cancelled the Telegram API calls of
// turns already in flight — the reply was computed, then the edit that would
// have shown it failed with "context canceled" and the owner was left looking
// at "Thinking..." forever.
//
// That made drain self-defeating: it waited (correctly) for in-flight turns to
// finish, having already guaranteed they could not deliver. Observed on
// 2026-08-08 09:13:37 when a source edit tripped the self-restart watcher on
// both agents while they were mid-answer.
//
// Detaching here restores the behaviour daemon.go always claimed: stopping the
// poller stops INTAKE, not delivery. A turn still ends — drain bounds the wait
// with its own timeout and proceeds regardless — but it is no longer severed
// mid-sentence by the act of draining.
func turnContext(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

func (b *Bot) defaultHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandleMessage(turnContext(ctx), tgBot, update.Message)
}

func (b *Bot) commandHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandleCommand(turnContext(ctx), tgBot, update.Message)
}

func (b *Bot) photoHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandlePhoto(turnContext(ctx), tgBot, update.Message)
}

func (b *Bot) stickerHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandleSticker(turnContext(ctx), tgBot, update.Message)
}

func (b *Bot) pdfHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandlePDF(turnContext(ctx), tgBot, update.Message)
}

func (b *Bot) reactionHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.MessageReaction == nil {
		return
	}
	b.handler.HandleReaction(turnContext(ctx), tgBot, update.MessageReaction)
}
