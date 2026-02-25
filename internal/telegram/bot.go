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

	b.bot = tgBot
	return b, nil
}

// Start begins long polling. Blocks until ctx is cancelled.
func (b *Bot) Start(ctx context.Context) {
	slog.Info("telegram bot starting long poll")
	b.bot.Start(ctx)
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
