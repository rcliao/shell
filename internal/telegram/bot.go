package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/rcliao/teeny-relay/internal/bridge"
)

type Bot struct {
	bot     *bot.Bot
	auth    *Auth
	bridge  *bridge.Bridge
	handler *Handler
}

func NewBot(token string, auth *Auth, br *bridge.Bridge) (*Bot, error) {
	if token == "" {
		return nil, fmt.Errorf("telegram bot token is empty")
	}

	b := &Bot{
		auth:   auth,
		bridge: br,
	}
	b.handler = NewHandler(auth, br)

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

	// Register handler for photo messages.
	tgBot.RegisterHandlerMatchFunc(
		func(update *models.Update) bool {
			return update.Message != nil && len(update.Message.Photo) > 0
		},
		b.photoHandler,
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

// SendText sends a message to a chat, splitting at paragraph boundaries if needed.
// Used for async notifications like plan progress.
func (b *Bot) SendText(chatID int64, text string) {
	ctx := context.Background()
	chunks := splitMessage(text, maxMessageLength)
	for _, chunk := range chunks {
		_, err := b.bot.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:    chatID,
			Text:      formatForMarkdownV2(chunk),
			ParseMode: models.ParseModeMarkdown,
		})
		if err != nil {
			slog.Warn("MarkdownV2 send failed, retrying as plain text", "error", err, "chat_id", chatID)
			_, err = b.bot.SendMessage(ctx, &bot.SendMessageParams{
				ChatID: chatID,
				Text:   chunk,
			})
			if err != nil {
				slog.Error("failed to send notification", "error", err, "chat_id", chatID)
			}
		}
	}
}

func (b *Bot) defaultHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandleMessage(ctx, tgBot, update.Message)
}

func (b *Bot) commandHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandleCommand(ctx, tgBot, update.Message)
}

func (b *Bot) photoHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.Message == nil {
		return
	}
	b.handler.HandlePhoto(ctx, tgBot, update.Message)
}

func (b *Bot) reactionHandler(ctx context.Context, tgBot *bot.Bot, update *models.Update) {
	if update.MessageReaction == nil {
		return
	}
	b.handler.HandleReaction(ctx, tgBot, update.MessageReaction)
}
