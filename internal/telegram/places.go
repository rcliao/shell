package telegram

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-telegram/bot"
)

// Places on Telegram (docs/DESIGN-PROJECT-AREAS.md): a project's own place is
// a forum topic in its chat. Topics have no tags, so stages are not shown.

// Telegram limits topic names to 128 characters.
const maxTopicName = 128

// CreateForumTopic opens a topic named title in a forum supergroup and posts
// content as its first message. It returns the topic's thread id.
func (b *Bot) CreateForumTopic(chatID int64, title, content string) (int64, error) {
	ctx := context.Background()
	name := []rune(title)
	if len(name) > maxTopicName {
		name = name[:maxTopicName]
	}
	var threadID int64
	err := withFloodRetry(ctx, func() error {
		t, err := b.bot.CreateForumTopic(ctx, &bot.CreateForumTopicParams{ChatID: chatID, Name: string(name)})
		if err == nil {
			threadID = int64(t.MessageThreadID)
		}
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("telegram: create forum topic: %w", err)
	}
	// The topic exists now: a failed first message must not orphan it (the
	// caller would leave the project unbound and a retry would open another).
	if content != "" {
		if err := b.SendTextButtons(chatID, threadID, content, nil); err != nil {
			slog.Warn("telegram: first message in new topic failed", "chat_id", chatID, "thread", threadID, "error", err)
		}
	}
	return threadID, nil
}

// CloseForumTopic closes a topic. It stays readable, and an admin can reopen it.
func (b *Bot) CloseForumTopic(chatID, threadID int64) error {
	ctx := context.Background()
	return withFloodRetry(ctx, func() error {
		_, err := b.bot.CloseForumTopic(ctx, &bot.CloseForumTopicParams{ChatID: chatID, MessageThreadID: int(threadID)})
		return err
	})
}
