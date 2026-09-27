package discord

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/rcliao/shell/internal/bridge"
)

// api is the slice of Discord the bot uses. The gateway session implements it
// in production; tests supply a fake, so the turn logic runs without a
// network.
type api interface {
	Send(channelID string, m *discordgo.MessageSend) (*discordgo.Message, error)
	Edit(m *discordgo.MessageEdit) (*discordgo.Message, error)
	Delete(channelID, messageID string) error
	React(channelID, messageID, emoji string) error
	Unreact(channelID, messageID, emoji string) error
	Typing(channelID string) error
	Pin(channelID, messageID string) error
	Unpin(channelID, messageID string) error
	// Channel returns channel metadata, from the gateway's cache when it has it.
	Channel(channelID string) (*discordgo.Channel, error)
}

// Bot is one agent's Discord presence: inbound turns through Handler, and the
// daemon's outbound sends (reminders, relays, the project home) through the
// methods below.
type Bot struct {
	api      api
	session  *discordgo.Session // nil in tests
	addr     *Addresses
	handler  *Handler
	dedup    func(chatID, threadID int64, text string) bool
	mentions *Mentions

	// Edits and pins name a message but not its channel (the outbound
	// interface was cut to Telegram, where chat + message id is enough). A
	// Discord edit needs the channel, so remember it for recent sends; a miss
	// falls back to the chat's main channel.
	chanMu      sync.Mutex
	msgChannel  map[int]string
	msgOrder    []int
	msgCapacity int
}

// Options wires a Bot to the rest of the daemon.
type Options struct {
	Addresses *Addresses
	Bridge    *bridge.Bridge
	Agent     AgentConfig
	// Authorize applies the existing (Telegram-keyed) access policy to a
	// linked person. Passed in so this package does not import telegram.
	Authorize func(userID, chatID int64, isGroup bool) bool
	// ProgressPhrasesPath is the agent's own progress-phrase file (the same
	// one Telegram reads); "" uses the built-in phrases.
	ProgressPhrasesPath string
	// Mentions turns "@name" into a Discord mention that notifies that
	// person; nil = no conversion.
	Mentions *Mentions
}

// NewBot opens nothing yet: Start connects the gateway.
func NewBot(token string, opts Options) (*Bot, error) {
	if token == "" {
		return nil, errors.New("discord: empty bot token")
	}
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("discord: %w", err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsGuildMessageReactions |
		discordgo.IntentsDirectMessageReactions
	s.StateEnabled = true

	b := newBot(sessionAPI{s}, opts)
	b.session = s
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		b.handler.selfID = r.User.ID
		slog.Info("discord: connected", "bot", r.User.Username, "guilds", len(r.Guilds))
	})
	// Turns run under a background context, not Start's: a drain-restart
	// closes the gateway, but a turn in flight must finish (the Telegram
	// handler detaches the same way). discordgo runs each handler in its own
	// goroutine, so a long turn never stalls the gateway.
	s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) {
		b.handler.HandleMessage(context.Background(), m.Message)
	})
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.MessageReactionAdd) {
		b.handler.HandleReaction(context.Background(), r.MessageReaction)
	})
	return b, nil
}

func newBot(a api, opts Options) *Bot {
	b := &Bot{
		api:         a,
		addr:        opts.Addresses,
		mentions:    opts.Mentions,
		msgChannel:  map[int]string{},
		msgCapacity: 5000,
	}
	b.handler = newHandler(a, b, opts)
	return b
}

// Start connects the gateway and blocks until ctx is cancelled, matching the
// daemon's transport lifecycle.
func (b *Bot) Start(ctx context.Context) {
	if b.session == nil {
		<-ctx.Done()
		return
	}
	// discordgo reconnects on its own once connected, but not after a failed
	// first Open — so a boot before the network is up would leave the family
	// unanswered until a restart. Retry with backoff instead.
	delay := 5 * time.Second
	for {
		err := b.session.Open()
		if err == nil {
			break
		}
		slog.Error("discord: gateway connect failed — retrying", "error", err, "in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 5*time.Minute {
			delay *= 2
		}
	}
	<-ctx.Done()
	if err := b.session.Close(); err != nil {
		slog.Warn("discord: close", "error", err)
	}
}

// Routes reports whether sends for this conversation belong to Discord.
func (b *Bot) Routes(chatID, threadID int64) bool {
	_, ok := b.addr.Outbound(chatID, threadID)
	return ok
}

// SetOutboundDedup installs the proactive-send dedup check.
func (b *Bot) SetOutboundDedup(check func(chatID, threadID int64, text string) bool) {
	b.dedup = check
}

func (b *Bot) target(chatID, threadID int64) (string, error) {
	t, ok := b.addr.Outbound(chatID, threadID)
	if !ok {
		return "", fmt.Errorf("discord: chat %d is not on discord", chatID)
	}
	return t.ChannelID, nil
}

// channelOf finds the channel a sent message lives in.
func (b *Bot) channelOf(chatID int64, messageID int) (string, error) {
	b.chanMu.Lock()
	ch, ok := b.msgChannel[messageID]
	b.chanMu.Unlock()
	if ok {
		return ch, nil
	}
	return b.target(chatID, 0)
}

func (b *Bot) remember(messageID int, channelID string) {
	b.chanMu.Lock()
	defer b.chanMu.Unlock()
	if _, ok := b.msgChannel[messageID]; !ok {
		b.msgOrder = append(b.msgOrder, messageID)
	}
	b.msgChannel[messageID] = channelID
	for len(b.msgOrder) > b.msgCapacity {
		delete(b.msgChannel, b.msgOrder[0])
		b.msgOrder = b.msgOrder[1:]
	}
}

// send posts one message and records where it went.
func (b *Bot) send(channelID string, m *discordgo.MessageSend) (int, error) {
	// A new message is the only thing that notifies: only the people the
	// agent named with "@name" are pinged.
	var users []string
	m.Content, users = b.mentions.Render(m.Content)
	m.AllowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users}
	sent, err := b.api.Send(channelID, m)
	if err != nil {
		return 0, err
	}
	id, err := strconv.Atoi(sent.ID)
	if err != nil {
		return 0, fmt.Errorf("discord: message id %q: %w", sent.ID, err)
	}
	b.remember(id, channelID)
	return id, nil
}

// sendChunks sends text as one or more messages; buttons ride the last one.
// Returns the ids of the messages that landed.
func (b *Bot) sendChunks(channelID, text string, buttons []bridge.LinkButton) ([]int, error) {
	if text == "" {
		text = "(empty response)"
	}
	chunks := splitMessage(fenceTables(text), maxMessageLen)
	var ids []int
	var lastErr error
	for i, c := range chunks {
		m := &discordgo.MessageSend{Content: c}
		if i == len(chunks)-1 {
			m.Components = linkButtons(buttons)
		}
		id, err := b.send(channelID, m)
		if err != nil {
			slog.Error("discord: send failed", "error", err, "channel", channelID)
			lastErr = err
			continue
		}
		ids = append(ids, id)
	}
	return ids, lastErr
}

func (b *Bot) SendText(chatID, threadID int64, text string) {
	_ = b.SendTextButtons(chatID, threadID, text, nil)
}

func (b *Bot) SendTextButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) error {
	if b.dedup != nil && b.dedup(chatID, threadID, text) {
		return nil
	}
	ch, err := b.target(chatID, threadID)
	if err != nil {
		slog.Error("discord: send dropped", "error", err)
		return err
	}
	_, err = b.sendChunks(ch, text, buttons)
	return err
}

func (b *Bot) sendFile(chatID, threadID int64, name string, data []byte, caption string) error {
	ch, err := b.target(chatID, threadID)
	if err != nil {
		return err
	}
	caption = truncateRunes(caption, maxMessageLen)
	_, err = b.send(ch, &discordgo.MessageSend{
		Content: caption,
		Files:   []*discordgo.File{{Name: name, Reader: bytes.NewReader(data)}},
	})
	return err
}

func (b *Bot) SendPhoto(chatID, threadID int64, data []byte, caption string) {
	if err := b.sendFile(chatID, threadID, "image"+imageExt(data), data, caption); err != nil {
		slog.Error("discord: photo send failed", "error", err, "chat_id", chatID)
	}
}

func (b *Bot) SendVideo(chatID, threadID int64, data []byte, caption string) {
	if err := b.sendFile(chatID, threadID, "video.mp4", data, caption); err != nil {
		slog.Error("discord: video send failed", "error", err, "chat_id", chatID)
	}
}

func (b *Bot) SendDocument(chatID, threadID int64, path, caption string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("discord: read document: %w", err)
	}
	return b.sendFile(chatID, threadID, filepath.Base(path), data, caption)
}

func (b *Bot) SendMessageID(chatID, threadID int64, text string) (int, error) {
	return b.SendMessageIDButtons(chatID, threadID, text, nil)
}

func (b *Bot) SendMessageIDButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) (int, error) {
	ch, err := b.target(chatID, threadID)
	if err != nil {
		return 0, err
	}
	// An id-returning send is edited in place later (the pinned project
	// home), so it must stay one message.
	return b.send(ch, &discordgo.MessageSend{
		Content:    truncateRunes(text, maxMessageLen),
		Components: linkButtons(buttons),
	})
}

func (b *Bot) EditMessage(chatID int64, messageID int, text string) error {
	return b.EditMessageButtons(chatID, messageID, text, nil)
}

func (b *Bot) EditMessageButtons(chatID int64, messageID int, text string, buttons []bridge.LinkButton) error {
	ch, err := b.channelOf(chatID, messageID)
	if err != nil {
		return err
	}
	content, _ := b.mentions.Render(truncateRunes(text, maxMessageLen))
	components := linkButtons(buttons)
	if components == nil {
		components = []discordgo.MessageComponent{} // clear old buttons
	}
	_, err = b.api.Edit(&discordgo.MessageEdit{
		ID: strconv.Itoa(messageID), Channel: ch,
		Content: &content, Components: &components,
		AllowedMentions: noPings(),
	})
	return err
}

func (b *Bot) PinMessage(chatID int64, messageID int, silent bool) error {
	ch, err := b.channelOf(chatID, messageID)
	if err != nil {
		return err
	}
	return b.api.Pin(ch, strconv.Itoa(messageID))
}

func (b *Bot) UnpinMessage(chatID int64, messageID int) error {
	ch, err := b.channelOf(chatID, messageID)
	if err != nil {
		return err
	}
	return b.api.Unpin(ch, strconv.Itoa(messageID))
}

// linkButtons renders URL buttons, five to a row (Discord's limit).
func linkButtons(buttons []bridge.LinkButton) []discordgo.MessageComponent {
	if len(buttons) == 0 {
		return nil
	}
	var rows []discordgo.MessageComponent
	var row []discordgo.MessageComponent
	for _, lb := range buttons {
		row = append(row, discordgo.Button{Label: truncateRunes(lb.Label, 80), Style: discordgo.LinkButton, URL: lb.URL})
		if len(row) == 5 {
			rows = append(rows, discordgo.ActionsRow{Components: row})
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: row})
	}
	return rows
}

// noPings stops the agent's text from notifying anyone: a reply that quotes
// "@everyone" or a user mention must not ping the family.
func noPings() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func imageExt(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG")):
		return ".png"
	case bytes.HasPrefix(data, []byte("GIF8")):
		return ".gif"
	case len(data) > 12 && string(data[8:12]) == "WEBP":
		return ".webp"
	default:
		return ".jpg"
	}
}

func truncateRunes(s string, max int) string {
	i := byteIndexOfRune(s, max)
	return s[:i]
}

// sessionAPI adapts the gateway session to api.
type sessionAPI struct{ s *discordgo.Session }

func (a sessionAPI) Send(ch string, m *discordgo.MessageSend) (*discordgo.Message, error) {
	return a.s.ChannelMessageSendComplex(ch, m)
}
func (a sessionAPI) Edit(m *discordgo.MessageEdit) (*discordgo.Message, error) {
	return a.s.ChannelMessageEditComplex(m)
}
func (a sessionAPI) Delete(ch, id string) error { return a.s.ChannelMessageDelete(ch, id) }
func (a sessionAPI) React(ch, id, emoji string) error {
	return a.s.MessageReactionAdd(ch, id, emoji)
}
func (a sessionAPI) Unreact(ch, id, emoji string) error {
	return a.s.MessageReactionRemove(ch, id, emoji, "@me")
}
func (a sessionAPI) Typing(ch string) error    { return a.s.ChannelTyping(ch) }
func (a sessionAPI) Pin(ch, id string) error   { return a.s.ChannelMessagePin(ch, id) }
func (a sessionAPI) Unpin(ch, id string) error { return a.s.ChannelMessageUnpin(ch, id) }
func (a sessionAPI) Channel(id string) (*discordgo.Channel, error) {
	if c, err := a.s.State.Channel(id); err == nil {
		return c, nil
	}
	return a.s.Channel(id)
}
