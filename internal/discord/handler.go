package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/progress"
)

// streamEditInterval paces streaming edits. Discord does not document its
// per-channel edit limit; community reports put it near 5 per 5 seconds, so
// 1.5 s leaves room for the status reactions that share the channel bucket.
const streamEditInterval = 1500 * time.Millisecond

// longRunningThreshold is when the 👀 receipt turns into ⏳.
const longRunningThreshold = 15 * time.Second

// maxAttachmentBytes caps what a turn downloads. Discord's own upload limit
// is lower for most users; this only stops a runaway download.
const maxAttachmentBytes = 50 << 20

// AgentConfig is the group-chat identity the handler needs, the Discord
// counterpart of telegram.AgentConfig.
type AgentConfig struct {
	Aliases              []string
	PeerAliases          []string
	GroupMode            string // "autonomous" = every message goes to the agent, which may [noop]
	BroadcastProbability float64
	// UserLabels maps a Telegram user id (the linked identity) to the label
	// used in the [From: ...] tag.
	UserLabels map[int64]string
}

// Handler turns Discord messages into bridge turns.
type Handler struct {
	api       api
	bot       *Bot
	addr      *Addresses
	bridge    *bridge.Bridge
	agent     AgentConfig
	authorize func(userID, chatID int64, isGroup bool) bool
	voice     *progress.Voice
	selfID    string
	http      *http.Client

	locksMu sync.Mutex
	locks   map[Conv]*sync.Mutex
}

func newHandler(a api, b *Bot, opts Options) *Handler {
	h := &Handler{
		api:       a,
		bot:       b,
		addr:      opts.Addresses,
		bridge:    opts.Bridge,
		agent:     opts.Agent,
		authorize: opts.Authorize,
		voice:     progress.New(opts.ProgressPhrasesPath),
		http:      &http.Client{Timeout: 60 * time.Second},
		locks:     map[Conv]*sync.Mutex{},
	}
	for i, a := range h.agent.Aliases {
		h.agent.Aliases[i] = strings.ToLower(a)
	}
	for i, a := range h.agent.PeerAliases {
		h.agent.PeerAliases[i] = strings.ToLower(a)
	}
	return h
}

// where describes the channel a message arrived in.
type where struct {
	conv      Conv
	channelID string
	isDM      bool
}

func (h *Handler) locate(channelID string) (where, error) {
	ch, err := h.api.Channel(channelID)
	if err != nil {
		return where{}, fmt.Errorf("channel %s: %w", channelID, err)
	}
	isDM := ch.Type == discordgo.ChannelTypeDM || ch.Type == discordgo.ChannelTypeGroupDM
	parent := ""
	if ch.IsThread() {
		parent = ch.ParentID
	}
	conv, err := h.addr.Inbound(channelID, parent, ch.GuildID, isDM)
	if err != nil {
		return where{}, err
	}
	return where{conv: conv, channelID: channelID, isDM: isDM}, nil
}

// HandleMessage runs one inbound message.
func (h *Handler) HandleMessage(ctx context.Context, m *discordgo.Message) {
	if m == nil || m.Author == nil || m.Author.ID == h.selfID {
		return
	}
	// v1 loop guard: other bots (including the peer agent) are not answered.
	// Discord, unlike Telegram, shows bots each other's messages; the peer
	// exchange limits in the Telegram handler can be ported when that is
	// wanted. Webhook posts carry a WebhookID and are skipped the same way.
	if m.Author.Bot || m.WebhookID != "" {
		return
	}
	w, err := h.locate(m.ChannelID)
	if err != nil {
		slog.Warn("discord: message from unknown channel", "channel", m.ChannelID, "error", err)
		return
	}
	userID, linked := h.addr.User(m.Author.ID)
	if !linked {
		slog.Info("discord: message from an unlinked user", "discord_user", m.Author.ID, "channel", m.ChannelID)
		if w.isDM {
			// The one place an unlinked person learns what to do; groups
			// stay quiet so a visitor cannot make the agent talk.
			h.reply(w.channelID, "I don't know you yet. Ask the owner to link your Discord id "+m.Author.ID+".")
		}
		return
	}
	isGroup := !w.isDM
	if h.authorize != nil && !h.authorize(userID, w.conv.ChatID, isGroup) {
		slog.Info("discord: access denied", "user_id", userID, "chat_id", w.conv.ChatID)
		return
	}

	msgID, err := strconv.Atoi(m.ID)
	if err != nil {
		slog.Warn("discord: bad message id", "id", m.ID)
		return
	}
	sender := h.label(userID, m.Author)
	text := strings.TrimSpace(h.stripSelfMention(m.Content))

	if isGroup {
		// Record what a person said before deciding whether to answer: a
		// message for the other agent is still something this one observed.
		h.bridge.RecordHumanMessage(w.conv.ChatID, w.conv.ThreadID, msgID, m.Timestamp, sender, transcriptText(m))
		if !h.shouldHandleGroup(m, text) {
			return
		}
	}

	if strings.HasPrefix(text, "/") && len(m.Attachments) == 0 {
		h.handleCommand(ctx, w, text)
		return
	}

	images, pdfs, text, ok := h.downloadAttachments(ctx, m, w, msgID, text)
	if !ok {
		h.setStatus(w.channelID, m.ID, "", "❌")
		return
	}
	images, text = h.stickerTurn(ctx, m, sender, text, images)
	defer cleanupMedia(images, pdfs)
	if text == "" {
		// Say so: a message that silently vanishes (a sticker did, before
		// stickers were handled) is invisible in every log.
		slog.Info("discord: message has nothing to answer", "chat_id", w.conv.ChatID, "msg_id", msgID,
			"attachments", len(m.Attachments), "stickers", len(m.StickerItems))
		return
	}
	h.runTurn(ctx, w, m, msgID, sender, text, images, pdfs, isGroup)
}

func (h *Handler) runTurn(ctx context.Context, w where, m *discordgo.Message, msgID int, sender, text string,
	images []bridge.ImageInfo, pdfs []bridge.PDFInfo, isGroup bool) {
	chat, thread := w.conv.ChatID, w.conv.ThreadID

	// Replay ledger: recorded before the turn so a restart can't eat it, and
	// a redelivered message that was already answered is skipped. Media
	// turns are left out (their temp files don't survive a restart), as on
	// Telegram.
	if len(images) == 0 && len(pdfs) == 0 {
		if done, err := h.bridge.BeginPendingTurn(chat, thread, msgID, sender, text); err == nil && done {
			slog.Info("discord: skipping already-answered message", "chat_id", chat, "msg_id", msgID)
			return
		}
	}
	start := time.Now()
	status := h.statusSetter(w.channelID, m.ID)
	status("👀")

	mu := h.lock(w.conv)
	if !mu.TryLock() {
		status("🕐")
		mu.Lock()
	}
	lockWait := time.Since(start)
	defer mu.Unlock()
	status("👀")
	longRunning := time.AfterFunc(longRunningThreshold, func() { status("⏳") })
	defer longRunning.Stop()

	placeholder, err := h.api.Send(w.channelID, &discordgo.MessageSend{
		Content:         h.voice.Thinking(0),
		Reference:       &discordgo.MessageReference{MessageID: m.ID, ChannelID: w.channelID},
		AllowedMentions: noPings(),
	})
	if err != nil {
		slog.Error("discord: placeholder failed — turn stays pending for replay", "error", err, "chat_id", chat)
		if aerr := h.bridge.AbandonPendingTurn(chat, msgID); aerr != nil {
			slog.Warn("discord: abandon pending turn", "error", aerr)
		}
		return
	}
	stopTyping := h.typing(w.channelID)
	defer stopTyping()

	s := newStreamer(h.api, w.channelID, placeholder.ID, h.voice)
	onEvent := func(ev process.StreamEvent) {
		switch e := ev.(type) {
		case process.TextDelta:
			s.append(e.Text)
		case process.ToolStarted:
			s.setTool(e.Name)
		case process.ToolFinished:
			s.setTool("")
		}
	}
	turnCtx := bridge.WithTelegramMsgID(ctx, msgID)
	resp, err := h.sendWithBusyRetry(turnCtx, chat, thread, text, sender, images, pdfs, onEvent)
	firstVisible := s.close()

	if err != nil {
		slog.Error("discord: turn failed", "error", err, "chat_id", chat)
		status("❌")
		msg := "Sorry, that didn't go through. Please try again."
		if errors.Is(err, context.DeadlineExceeded) {
			msg = "That took too long and timed out. Please try again, or ask for something smaller."
		}
		h.editText(w.channelID, placeholder.ID, msg)
		return
	}

	response := resp.Text
	noMedia := len(resp.Photos) == 0 && len(resp.Videos) == 0 && len(resp.Documents) == 0
	if isGroup && h.agent.GroupMode == "autonomous" && response == "" && noMedia {
		slog.Info("discord: autonomous noop — agent chose not to speak", "chat_id", chat)
		_ = h.api.Delete(w.channelID, placeholder.ID)
		status("")
		h.complete(chat, msgID)
		return
	}
	for _, p := range resp.Photos {
		h.bot.SendPhoto(chat, thread, p.Data, p.Caption)
	}
	for _, v := range resp.Videos {
		h.bot.SendVideo(chat, thread, v.Data, v.Caption)
	}
	for _, d := range resp.Documents {
		if err := h.bot.SendDocument(chat, thread, d.Path, d.Caption); err != nil {
			slog.Error("discord: document send failed", "error", err)
		}
	}
	if response == "" && !isGroup && noMedia {
		// An empty reply is never right in a DM: it was addressed to the agent.
		slog.Warn("discord: empty DM reply — retrying with a corrective note", "chat_id", chat)
		retry, rerr := h.bridge.HandleMessageStreamingEvents(turnCtx, chat, thread,
			"[system: your reply to the user's last message came back empty or [noop]. That is never valid in a direct chat — the message was addressed to you. Answer it now; if it is ambiguous, ask a brief clarifying question.]",
			sender, nil, nil, nil)
		if rerr == nil {
			response = retry.Text
		}
	}
	if response == "" {
		response = "(empty response)"
	}

	var botIDs []int
	if len([]rune(fenceTables(response))) <= maxMessageLen {
		h.editText(w.channelID, placeholder.ID, fenceTables(response))
		if id, err := strconv.Atoi(placeholder.ID); err == nil {
			h.bot.remember(id, w.channelID)
			botIDs = []int{id}
		}
	} else {
		if err := h.api.Delete(w.channelID, placeholder.ID); err != nil {
			slog.Warn("discord: delete placeholder before chunked reply", "error", err)
		}
		botIDs, _ = h.bot.sendChunks(w.channelID, response, nil)
	}
	for _, id := range botIDs {
		if err := h.bridge.SaveMessageMap(chat, thread, msgID, id, text, response); err != nil {
			slog.Warn("discord: save message map", "error", err)
		}
	}
	h.complete(chat, msgID)

	total := time.Since(start)
	fv := total
	if !firstVisible.IsZero() {
		fv = firstVisible.Sub(start)
	}
	h.bridge.SaveTurnE2E(chat, msgID, 0, lockWait.Milliseconds(), fv.Milliseconds(), total.Milliseconds())
	slog.Info("discord: turn done", "chat_id", chat, "thread_id", thread,
		"first_visible_s", fv.Seconds(), "total_s", total.Seconds(), "chunks", len(botIDs))

	if looksLikeQuestion(response) {
		status("🤔")
	} else {
		status("✅")
	}
}

func (h *Handler) complete(chat int64, msgID int) {
	if err := h.bridge.CompletePendingTurn(chat, msgID); err != nil {
		slog.Warn("discord: complete pending turn", "error", err, "chat_id", chat)
	}
}

// userBusyRetryDelays matches the Telegram handler: a user message that finds
// the session held by a synthetic turn (A2A, scheduler) waits it out rather
// than being dropped.
var userBusyRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second}

func (h *Handler) sendWithBusyRetry(ctx context.Context, chat, thread int64, text, sender string,
	images []bridge.ImageInfo, pdfs []bridge.PDFInfo, onEvent process.EventFunc) (bridge.AgentResponse, error) {
	resp, err := h.bridge.HandleMessageStreamingEvents(ctx, chat, thread, text, sender, images, pdfs, onEvent)
	for _, d := range userBusyRetryDelays {
		if !errors.Is(err, process.ErrSessionBusy) {
			return resp, err
		}
		slog.Info("discord: session busy, retrying", "chat_id", chat, "delay", d)
		select {
		case <-ctx.Done():
			return resp, err
		case <-time.After(d):
		}
		resp, err = h.bridge.HandleMessageStreamingEvents(ctx, chat, thread, text, sender, images, pdfs, onEvent)
	}
	return resp, err
}

func (h *Handler) handleCommand(ctx context.Context, w where, text string) {
	parts := strings.SplitN(strings.TrimPrefix(text, "/"), " ", 2)
	cmd := parts[0]
	args := ""
	if len(parts) > 1 {
		args = parts[1]
	}
	resp, err := h.bridge.HandleCommand(ctx, w.conv.ChatID, w.conv.ThreadID, cmd, args)
	if err != nil {
		slog.Error("discord: command failed", "cmd", cmd, "error", err)
		resp = "Command failed: " + err.Error()
	}
	h.bot.sendChunks(w.channelID, resp, nil)
}

// HandleReaction routes a person's reaction on one of this agent's replies
// (👍 remember, 🔄 regenerate, ...) to the bridge. Reactions on anything else
// — family members reacting to each other — say nothing about the agent.
func (h *Handler) HandleReaction(ctx context.Context, r *discordgo.MessageReaction) {
	if r == nil || r.UserID == h.selfID || r.Emoji.ID != "" { // custom server emoji carry no action
		return
	}
	userID, linked := h.addr.User(r.UserID)
	if !linked {
		return
	}
	w, err := h.locate(r.ChannelID)
	if err != nil {
		return
	}
	if h.authorize != nil && !h.authorize(userID, w.conv.ChatID, !w.isDM) {
		return
	}
	msgID, err := strconv.Atoi(r.MessageID)
	if err != nil {
		return
	}
	mm, err := h.bridge.GetMessageMapByBotMsg(w.conv.ChatID, msgID)
	if err != nil || mm == nil {
		return
	}
	emoji := r.Emoji.Name
	thread := w.conv.ThreadID
	h.bridge.LogReaction(w.conv.ChatID, thread, msgID, emoji)
	if h.bridge.ReactionAction(emoji) == "" {
		return // plain feedback: logged, no reply
	}
	resp, err := h.bridge.HandleReaction(ctx, w.conv.ChatID, thread, msgID, emoji)
	if err != nil {
		slog.Error("discord: reaction failed", "error", err, "emoji", emoji)
		h.mark(w.channelID, r.MessageID, "❌")
		return
	}
	// The result mark tells the person their reaction did something: 📌 on
	// a reply is otherwise silent when remembering succeeds.
	h.mark(w.channelID, r.MessageID, "✅")
	if resp != "" {
		h.bot.sendChunks(w.channelID, resp, nil)
	}
}

// shouldHandleGroup applies the group addressing rules: a mention of this bot
// or a reply to it is always handled; a message for another bot, or opening
// with the peer agent's name, is left to them; otherwise autonomous mode hands
// everything to the agent, which may stay silent.
func (h *Handler) shouldHandleGroup(m *discordgo.Message, text string) bool {
	if m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil && m.ReferencedMessage.Author.ID == h.selfID {
		return true
	}
	mentionsPeer := false
	for _, u := range m.Mentions {
		if u.ID == h.selfID {
			return true
		}
		if u.Bot {
			mentionsPeer = true
		}
	}
	if mentionsPeer {
		return false
	}
	me := addressedTo(text, h.agent.Aliases)
	if namedIn(text, h.agent.Aliases) && namedIn(text, h.agent.PeerAliases) {
		return true
	}
	if addressedTo(text, h.agent.PeerAliases) && !me {
		return false
	}
	if h.agent.GroupMode == "autonomous" {
		return true
	}
	p := h.agent.BroadcastProbability
	return p >= 1 || (p > 0 && rand.Float64() < p)
}

var addressLeadRe = regexp.MustCompile(`^[\s\p{P}\p{S}]+`)

// addressedTo reports whether text opens by naming one of the aliases.
func addressedTo(text string, aliases []string) bool {
	lower := strings.ToLower(addressLeadRe.ReplaceAllString(text, ""))
	for _, a := range aliases {
		if a != "" && strings.HasPrefix(lower, a) {
			return true
		}
	}
	return false
}

// namedIn reports whether any alias appears anywhere in text.
func namedIn(text string, aliases []string) bool {
	lower := strings.ToLower(text)
	for _, a := range aliases {
		if a != "" && strings.Contains(lower, a) {
			return true
		}
	}
	return false
}

func (h *Handler) stripSelfMention(s string) string {
	if h.selfID == "" {
		return s
	}
	s = strings.ReplaceAll(s, "<@"+h.selfID+">", "")
	return strings.ReplaceAll(s, "<@!"+h.selfID+">", "")
}

// label is the [From: ...] name: the configured label for the linked person,
// else their Discord display name.
func (h *Handler) label(userID int64, u *discordgo.User) string {
	if l := h.agent.UserLabels[userID]; l != "" {
		return l
	}
	if u.GlobalName != "" {
		return u.GlobalName
	}
	return u.Username
}

func (h *Handler) lock(c Conv) *sync.Mutex {
	h.locksMu.Lock()
	defer h.locksMu.Unlock()
	mu, ok := h.locks[c]
	if !ok {
		mu = &sync.Mutex{}
		h.locks[c] = mu
	}
	return mu
}

// statusSetter returns a function that shows one status reaction on the
// user's message. Discord adds reactions rather than replacing them, so the
// previous one is removed first — otherwise 👀 ⏳ ✅ pile up. "" clears it.
func (h *Handler) statusSetter(channelID, messageID string) func(string) {
	var mu sync.Mutex
	current := ""
	return func(emoji string) {
		mu.Lock()
		defer mu.Unlock()
		h.setStatus(channelID, messageID, current, emoji)
		current = emoji
	}
}

// mark adds a result reaction to a message (the agent's reply a person
// reacted to).
func (h *Handler) mark(channelID, messageID, emoji string) {
	if err := h.api.React(channelID, messageID, emoji); err != nil {
		slog.Debug("discord: result mark", "error", err)
	}
}

func (h *Handler) setStatus(channelID, messageID, prev, next string) {
	if prev == next {
		return
	}
	if prev != "" {
		if err := h.api.Unreact(channelID, messageID, prev); err != nil {
			slog.Debug("discord: remove reaction", "error", err)
		}
	}
	if next != "" {
		if err := h.api.React(channelID, messageID, next); err != nil {
			slog.Debug("discord: add reaction", "error", err)
		}
	}
}

// typing keeps the typing indicator up (it lasts ~10 s per call) until the
// returned stop is called.
func (h *Handler) typing(channelID string) func() {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(8 * time.Second)
		defer t.Stop()
		for {
			_ = h.api.Typing(channelID)
			select {
			case <-done:
				return
			case <-t.C:
			}
		}
	}()
	return func() { close(done) }
}

func (h *Handler) reply(channelID, text string) {
	if _, err := h.api.Send(channelID, &discordgo.MessageSend{Content: text, AllowedMentions: noPings()}); err != nil {
		slog.Warn("discord: reply failed", "error", err)
	}
}

func (h *Handler) editText(channelID, messageID, text string) {
	content := truncateRunes(text, maxMessageLen)
	if _, err := h.api.Edit(&discordgo.MessageEdit{ID: messageID, Channel: channelID, Content: &content, AllowedMentions: noPings()}); err != nil {
		slog.Warn("discord: edit failed", "error", err, "message", messageID)
	}
}

// downloadAttachments fetches images and PDFs for the turn. It returns the
// text to send (a caption stand-in when the message had no words) and false
// when a download failed and the turn should not run.
func (h *Handler) downloadAttachments(ctx context.Context, m *discordgo.Message, w where, msgID int, text string) ([]bridge.ImageInfo, []bridge.PDFInfo, string, bool) {
	var images []bridge.ImageInfo
	var pdfs []bridge.PDFInfo
	for _, a := range m.Attachments {
		ct := strings.ToLower(a.ContentType)
		switch {
		case strings.HasPrefix(ct, "image/"):
			path, size, err := h.download(ctx, a, "dc-photo-*"+extFor(a.Filename, ".jpg"))
			if err != nil {
				slog.Error("discord: image download failed", "error", err)
				cleanupMedia(images, pdfs)
				return nil, nil, text, false
			}
			img := bridge.ImageInfo{Path: path, Width: a.Width, Height: a.Height, Size: size}
			h.bridge.ArchiveInboundMedia(w.conv.ChatID, w.conv.ThreadID, msgID, text, &img)
			images = append(images, img)
		case ct == "application/pdf":
			path, size, err := h.download(ctx, a, "dc-pdf-*.pdf")
			if err != nil {
				slog.Error("discord: pdf download failed", "error", err)
				cleanupMedia(images, pdfs)
				return nil, nil, text, false
			}
			pdfs = append(pdfs, bridge.PDFInfo{Path: path, Size: size})
		}
	}
	if text == "" {
		switch {
		case len(images) > 0:
			text = "(photo)"
		case len(pdfs) > 0:
			text = "(pdf)"
		}
	}
	return images, pdfs, text, true
}

func (h *Handler) download(ctx context.Context, a *discordgo.MessageAttachment, pattern string) (string, int64, error) {
	if a.Size > maxAttachmentBytes {
		return "", 0, fmt.Errorf("attachment %s is %d bytes, over the %d cap", a.Filename, a.Size, maxAttachmentBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := h.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("download %s: status %d", a.Filename, resp.StatusCode)
	}
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", 0, err
	}
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxAttachmentBytes))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	return tmp.Name(), n, nil
}

// stickerURL is where Discord serves a sticker's image. A variable so tests
// can point it at a local server.
var stickerURL = func(id, ext string) string {
	return "https://media.discordapp.net/stickers/" + id + "." + ext
}

// stickerTurn turns a sticker into something the agent can read, the way the
// Telegram handler does: the sender and sticker name in words, and the image
// when there is one. A sticker-only message used to arrive with empty text and
// be dropped without a trace. Lottie stickers are vector animations with no
// image form, so they are described in words only.
func (h *Handler) stickerTurn(ctx context.Context, m *discordgo.Message, sender, text string, images []bridge.ImageInfo) ([]bridge.ImageInfo, string) {
	for _, st := range m.StickerItems {
		if st == nil {
			continue
		}
		desc := "(sticker from " + sender + ": " + st.Name + ")"
		if text == "" {
			text = desc
		} else {
			text += " [sticker: " + st.Name + "]"
		}
		ext := ""
		switch st.FormatType {
		case discordgo.StickerFormatTypePNG, discordgo.StickerFormatTypeAPNG:
			ext = "png"
		case discordgo.StickerFormatTypeGIF:
			ext = "gif"
		default:
			text += " [animated sticker, no image]"
			continue
		}
		if len(images) > 0 {
			continue // a photo in the same message says more than the sticker
		}
		path, size, err := h.download(ctx, &discordgo.MessageAttachment{URL: stickerURL(st.ID, ext), Filename: st.Name + "." + ext}, "dc-sticker-*."+ext)
		if err != nil {
			slog.Warn("discord: sticker image download failed, sending words only", "error", err, "sticker", st.ID)
			continue
		}
		images = append(images, bridge.ImageInfo{Path: path, Size: size})
	}
	return images, text
}

// cleanupMedia removes downloaded temp files. Archived images were moved to
// the media store (MediaID set) and are kept.
func cleanupMedia(images []bridge.ImageInfo, pdfs []bridge.PDFInfo) {
	for _, img := range images {
		if img.MediaID == 0 {
			os.Remove(img.Path)
		}
	}
	for _, p := range pdfs {
		os.Remove(p.Path)
	}
}

func extFor(name, def string) string {
	if e := strings.ToLower(filepath.Ext(name)); e != "" && len(e) <= 5 {
		return e
	}
	return def
}

// transcriptText is what a group message contributes to the shared
// transcript: its words, else a marker for what was attached.
func transcriptText(m *discordgo.Message) string {
	if t := strings.TrimSpace(m.Content); t != "" {
		return t
	}
	if len(m.Attachments) > 0 {
		return "(attachment)"
	}
	for _, st := range m.StickerItems {
		if st != nil {
			return "(sticker: " + st.Name + ")"
		}
	}
	return ""
}

func looksLikeQuestion(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？")
}

// progressInterval is how often the placeholder's progress phrase changes
// before the first words arrive — the same 2 s cadence as Telegram.
const progressInterval = 2 * time.Second

// streamer owns every edit of the placeholder. Before the reply's first
// words it shows the agent's progress voice (what it is thinking or which
// kind of tool it is using); after, it flushes the growing reply at most once
// per streamEditInterval. One goroutine does both, so a late progress edit
// can never overwrite streamed text.
type streamer struct {
	api       api
	channelID string
	messageID string
	voice     *progress.Voice

	mu           sync.Mutex
	text         strings.Builder
	tool         string
	firstVisible time.Time
	dirty        chan struct{}
	done         chan struct{}
}

func newStreamer(a api, channelID, messageID string, voice *progress.Voice) *streamer {
	s := &streamer{api: a, channelID: channelID, messageID: messageID, voice: voice,
		dirty: make(chan struct{}, 1), done: make(chan struct{})}
	go s.loop(progressInterval)
	return s
}

func (s *streamer) append(delta string) {
	s.mu.Lock()
	s.text.WriteString(delta)
	s.mu.Unlock()
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// setTool records the tool now running ("" when it finished), for the
// progress phrase.
func (s *streamer) setTool(name string) {
	s.mu.Lock()
	s.tool = name
	s.mu.Unlock()
}

func (s *streamer) edit(content string) error {
	_, err := s.api.Edit(&discordgo.MessageEdit{ID: s.messageID, Channel: s.channelID, Content: &content, AllowedMentions: noPings()})
	return err
}

func (s *streamer) loop(every time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	tick := 0
	last := ""
	var lastEdit time.Time
	for {
		select {
		case _, ok := <-s.dirty:
			if !ok {
				return
			}
			if !lastEdit.IsZero() {
				if wait := streamEditInterval - time.Since(lastEdit); wait > 0 {
					time.Sleep(wait)
				}
			}
			s.mu.Lock()
			cur := s.text.String()
			s.mu.Unlock()
			if cur == last || strings.TrimSpace(cur) == "" {
				continue
			}
			if err := s.edit(streamView(cur, maxMessageLen)); err != nil {
				slog.Debug("discord: streaming edit failed", "error", err)
				continue
			}
			last = cur
			lastEdit = time.Now()
			s.mu.Lock()
			if s.firstVisible.IsZero() {
				s.firstVisible = lastEdit
			}
			s.mu.Unlock()
		case <-ticker.C:
			s.mu.Lock()
			started := s.text.Len() > 0
			tool := s.tool
			s.mu.Unlock()
			if started {
				ticker.Stop() // the reply itself is the progress now
				continue
			}
			tick++
			msg := s.voice.Thinking(tick)
			if tool != "" {
				msg = s.voice.Tool(tick, tool)
			}
			if err := s.edit(msg); err != nil {
				slog.Debug("discord: progress edit failed", "error", err)
			}
		}
	}
}

// close stops streaming and returns when the first words became visible
// (zero if nothing streamed). The final text is written by the caller.
func (s *streamer) close() time.Time {
	close(s.dirty)
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstVisible
}
