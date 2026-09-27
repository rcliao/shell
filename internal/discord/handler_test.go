package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"github.com/rcliao/shell/internal/bridge"
	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/progress"
	"github.com/rcliao/shell/internal/store"
)

// fakeAPI records what the bot did on Discord.
type fakeAPI struct {
	mu       sync.Mutex
	channels map[string]*discordgo.Channel
	nextID   int64
	sends    []sent
	edits    map[string]string // message id → latest content
	deletes  []string
	reacts   []string // "+emoji" / "-emoji"
}

type sent struct {
	channel, id, content string
	files                int
	buttons              int
	pings                []string
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{channels: map[string]*discordgo.Channel{}, nextID: 400000000000000000, edits: map[string]string{}}
}

func (f *fakeAPI) Send(ch string, m *discordgo.MessageSend) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.FormatInt(f.nextID, 10)
	var pings []string
	if m.AllowedMentions != nil {
		pings = m.AllowedMentions.Users
	}
	f.sends = append(f.sends, sent{channel: ch, id: id, content: m.Content, files: len(m.Files), buttons: len(m.Components), pings: pings})
	return &discordgo.Message{ID: id, ChannelID: ch}, nil
}
func (f *fakeAPI) Edit(m *discordgo.MessageEdit) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Content != nil {
		f.edits[m.ID] = *m.Content
	}
	return &discordgo.Message{ID: m.ID, ChannelID: m.Channel}, nil
}
func (f *fakeAPI) Delete(ch, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, id)
	return nil
}
func (f *fakeAPI) React(ch, id, e string) error {
	f.mu.Lock()
	f.reacts = append(f.reacts, "+"+e)
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) Unreact(ch, id, e string) error {
	f.mu.Lock()
	f.reacts = append(f.reacts, "-"+e)
	f.mu.Unlock()
	return nil
}
func (f *fakeAPI) Typing(string) error        { return nil }
func (f *fakeAPI) Pin(string, string) error   { return nil }
func (f *fakeAPI) Unpin(string, string) error { return nil }
func (f *fakeAPI) StartForumThread(forum string, t *discordgo.ThreadStart, m *discordgo.MessageSend) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.FormatInt(f.nextID, 10)
	th := &discordgo.Channel{ID: id, ParentID: forum, Name: t.Name, Type: discordgo.ChannelTypeGuildPublicThread, AppliedTags: t.AppliedTags}
	f.channels[id] = th
	f.sends = append(f.sends, sent{channel: id, content: m.Content})
	return th, nil
}

func (f *fakeAPI) EditChannel(id string, e *discordgo.ChannelEdit) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.channels[id]
	if !ok {
		return nil, discordgo.ErrStateNotFound
	}
	if e.AvailableTags != nil {
		// Discord assigns ids to new tags.
		tags := append([]discordgo.ForumTag(nil), (*e.AvailableTags)...)
		for i := range tags {
			if tags[i].ID == "" {
				f.nextID++
				tags[i].ID = strconv.FormatInt(f.nextID, 10)
			}
		}
		c.AvailableTags = tags
	}
	if e.AppliedTags != nil {
		c.AppliedTags = *e.AppliedTags
	}
	if e.Archived != nil {
		if c.ThreadMetadata == nil {
			c.ThreadMetadata = &discordgo.ThreadMetadata{}
		}
		c.ThreadMetadata.Archived = *e.Archived
	}
	return c, nil
}

func (f *fakeAPI) Channel(id string) (*discordgo.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.channels[id]; ok {
		return c, nil
	}
	return nil, discordgo.ErrStateNotFound
}

// fakeAgent answers every turn with a fixed reply, streamed as one delta.
type fakeAgent struct {
	process.Agent
	mu    sync.Mutex
	reply string
	turns []process.AgentRequest
}

func (a *fakeAgent) Send(ctx context.Context, req process.AgentRequest, on process.StreamFunc) (process.SendResult, error) {
	return a.SendEvents(ctx, req, func(ev process.StreamEvent) {
		if d, ok := ev.(process.TextDelta); ok && on != nil {
			on(d.Text)
		}
	})
}
func (a *fakeAgent) SendEvents(ctx context.Context, req process.AgentRequest, emit process.EventFunc) (process.SendResult, error) {
	a.mu.Lock()
	a.turns = append(a.turns, req)
	reply := a.reply
	a.mu.Unlock()
	if emit != nil {
		emit(process.TextDelta{Text: reply})
	}
	return process.SendResult{Text: reply, TextSegments: []string{reply}, SessionID: "sess-1"}, nil
}
func (a *fakeAgent) turnCount() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.turns) }

const (
	selfBot    = "500000000000000001"
	peerBot    = "500000000000000002"
	linkedUser = "300000000000000001"
	strangerID = "300000000000000009"
	dmChan     = "200000000000000003" // linked to Telegram DM 42
	groupChan  = "200000000000000001" // linked to Telegram group -100200300
)

type harness struct {
	api   *fakeAPI
	agent *fakeAgent
	bot   *Bot
	br    *bridge.Bridge
}

func newHarness(t *testing.T, reply string) *harness {
	t.Helper()
	t.Setenv("GHOST_EMBED_PROVIDER", "none")
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	agent := &fakeAgent{Agent: process.NewManager(process.ManagerConfig{Binary: "echo"}), reply: reply}
	br := bridge.New(agent, s, nil, nil, false, "", map[string]string{"❌": "cancel"}, nil, nil, nil)

	api := newFakeAPI()
	api.channels[dmChan] = &discordgo.Channel{ID: dmChan, Type: discordgo.ChannelTypeDM}
	api.channels[groupChan] = &discordgo.Channel{ID: groupChan, Type: discordgo.ChannelTypeGuildText}
	api.channels["200000000000000013"] = &discordgo.Channel{ID: "200000000000000013", Type: discordgo.ChannelTypeGuildText, GuildID: "200000000000000900"}
	addr := NewAddresses(config.DiscordConfig{
		Users: map[string]int64{linkedUser: 42},
		Chats: map[string]config.DiscordChatLink{
			dmChan:    {ChatID: 42},
			groupChan: {ChatID: -100200300},
		},
		Guilds: map[string]config.DiscordGuildLink{"200000000000000900": {ChatID: -100200300}},
	})
	b := newBot(api, Options{
		Mentions: NewMentions(map[string]string{"owner": linkedUser}),
		Addresses: addr,
		Bridge:    br,
		Agent: AgentConfig{
			Aliases: []string{"pika"}, PeerAliases: []string{"umbreon"},
			GroupMode: "autonomous", UserLabels: map[int64]string{42: "The Owner"},
		},
		Authorize: func(userID, chatID int64, isGroup bool) bool { return userID == 42 },
	})
	b.handler.selfID = selfBot
	return &harness{api: api, agent: agent, bot: b, br: br}
}

func msg(id, channel, author, content string) *discordgo.Message {
	return &discordgo.Message{ID: id, ChannelID: channel, Content: content, Timestamp: time.Now(),
		Author: &discordgo.User{ID: author, Username: "someone"}}
}

func TestDMTurnStreamsAndFinalizes(t *testing.T) {
	h := newHarness(t, "Hello from the agent.")
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000001", dmChan, linkedUser, "hi"))

	if h.agent.turnCount() != 1 {
		t.Fatalf("agent turns = %d, want 1", h.agent.turnCount())
	}
	req := h.agent.turns[0]
	if req.ChatID != 42 {
		t.Fatalf("turn ran in chat %d, want the linked Telegram DM 42", req.ChatID)
	}
	if len(h.api.sends) != 1 || !strings.Contains(h.api.sends[0].content, "Thinking") || h.api.sends[0].channel != dmChan {
		t.Fatalf("want one placeholder in the DM, got %+v", h.api.sends)
	}
	placeholder := h.api.sends[0].id
	if got := h.api.edits[placeholder]; got != "Hello from the agent." {
		t.Fatalf("placeholder final text = %q", got)
	}
	reacts := strings.Join(h.api.reacts, " ")
	if !strings.HasPrefix(reacts, "+👀") || !strings.HasSuffix(reacts, "-👀 +✅") {
		t.Fatalf("status reactions = %q, want 👀 replaced by ✅", reacts)
	}
	id, _ := strconv.Atoi(placeholder)
	mm, err := h.br.GetMessageMapByBotMsg(42, id)
	if err != nil || mm == nil {
		t.Fatalf("message map not saved: %v", err)
	}
	if !strings.Contains(req.Text, "The Owner") && !strings.Contains(req.Text, "hi") {
		t.Fatalf("turn text %q lost the message", req.Text)
	}
}

func TestUnlinkedUserIsToldHowToLinkInDMOnly(t *testing.T) {
	h := newHarness(t, "should not run")
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000002", dmChan, strangerID, "hello?"))
	if h.agent.turnCount() != 0 {
		t.Fatal("an unlinked person must not reach the agent")
	}
	if len(h.api.sends) != 1 || !strings.Contains(h.api.sends[0].content, strangerID) {
		t.Fatalf("want one reply naming the id to link, got %+v", h.api.sends)
	}

	h2 := newHarness(t, "should not run")
	h2.bot.handler.HandleMessage(context.Background(), msg("600000000000000003", groupChan, strangerID, "hello?"))
	if h2.agent.turnCount() != 0 || len(h2.api.sends) != 0 {
		t.Fatal("in a group an unlinked person gets silence")
	}
}

func TestBotsAreIgnored(t *testing.T) {
	h := newHarness(t, "no")
	m := msg("600000000000000004", groupChan, peerBot, "pika, what do you think?")
	m.Author.Bot = true
	h.bot.handler.HandleMessage(context.Background(), m)
	if h.agent.turnCount() != 0 || len(h.api.sends) != 0 {
		t.Fatal("the peer agent's messages must not start a turn (loop guard)")
	}
}

func TestGroupAddressing(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		mentions []*discordgo.User
		want     bool
	}{
		{"autonomous: plain message goes to the agent", "what's for dinner", nil, true},
		{"addressed to the peer by name", "umbreon, what's for dinner", nil, false},
		{"mentions the peer bot", "<@" + peerBot + "> dinner?", []*discordgo.User{{ID: peerBot, Bot: true}}, false},
		{"mentions this bot", "<@" + selfBot + "> dinner?", []*discordgo.User{{ID: selfBot, Bot: true}}, true},
		{"both named", "umbreon and pika, dinner?", nil, true},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "ok")
			m := msg("60000000000000010"+strconv.Itoa(i), groupChan, linkedUser, c.content)
			m.Mentions = c.mentions
			h.bot.handler.HandleMessage(context.Background(), m)
			if got := h.agent.turnCount() == 1; got != c.want {
				t.Fatalf("handled = %v, want %v", got, c.want)
			}
			if c.want && h.agent.turns[0].ChatID != -100200300 {
				t.Fatalf("group turn in chat %d, want the linked group", h.agent.turns[0].ChatID)
			}
			if c.want && strings.Contains(h.agent.turns[0].Text, "<@"+selfBot+">") {
				t.Fatal("the bot's own mention must be stripped from the text")
			}
		})
	}
}

func TestLongReplyIsChunked(t *testing.T) {
	long := strings.Repeat("A long paragraph of the answer. ", 150) // ~4800 chars
	h := newHarness(t, long)
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000020", dmChan, linkedUser, "tell me everything"))

	if len(h.api.deletes) != 1 || h.api.deletes[0] != h.api.sends[0].id {
		t.Fatalf("the placeholder should be deleted before a chunked reply, deletes=%v", h.api.deletes)
	}
	chunks := h.api.sends[1:]
	if len(chunks) < 3 {
		t.Fatalf("want ≥3 chunks for ~4800 chars, got %d", len(chunks))
	}
	var joined strings.Builder
	for _, c := range chunks {
		if n := len([]rune(c.content)); n > maxMessageLen {
			t.Fatalf("chunk of %d chars exceeds Discord's limit", n)
		}
		joined.WriteString(c.content)
	}
	if joined.String() != strings.TrimSpace(long) {
		t.Fatal("chunks do not add up to the reply")
	}
	for _, c := range chunks {
		id, _ := strconv.Atoi(c.id)
		if mm, _ := h.br.GetMessageMapByBotMsg(42, id); mm == nil {
			t.Fatalf("chunk %s not in the message map (reactions on it would be lost)", c.id)
		}
	}
}

func TestTextCommandRunsWithoutATurn(t *testing.T) {
	h := newHarness(t, "no turn")
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000030", dmChan, linkedUser, "/help"))
	if h.agent.turnCount() != 0 {
		t.Fatal("/help is a command, not a turn")
	}
	if len(h.api.sends) == 0 {
		t.Fatal("the command reply was not sent")
	}
}

func TestOutboundSendsAndEditsInTheRightChannel(t *testing.T) {
	h := newHarness(t, "")
	h.bot.SendText(-100200300, 0, "reminder: dinner at 6")
	if len(h.api.sends) != 1 || h.api.sends[0].channel != groupChan {
		t.Fatalf("reminder for the linked group went to %+v", h.api.sends)
	}
	// A thread message is edited in the thread, not the main channel.
	thread := "200000000000000007"
	id, err := h.bot.SendMessageIDButtons(-100200300, 200000000000000007, "home", []bridge.LinkButton{{Label: "doc", URL: "https://example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if last := h.api.sends[len(h.api.sends)-1]; last.channel != thread || last.buttons != 1 {
		t.Fatalf("thread send = %+v", last)
	}
	if err := h.bot.EditMessage(-100200300, id, "home v2"); err != nil {
		t.Fatal(err)
	}
	if h.api.edits[strconv.Itoa(id)] != "home v2" {
		t.Fatal("edit did not land")
	}
	if err := h.bot.SendTextButtons(-100999, 0, "not on discord", nil); err == nil {
		t.Fatal("a Telegram-only chat must be refused, not guessed")
	}
}

func startTestStreamer(api *fakeAPI) *streamer {
	s := &streamer{api: api, channelID: dmChan, messageID: "900000000000000001", voice: progress.New(""),
		dirty: make(chan struct{}, 1), done: make(chan struct{})}
	go s.loop(10 * time.Millisecond)
	return s
}

func TestStreamerShowsProgressUntilFirstWords(t *testing.T) {
	api := newFakeAPI()
	s := startTestStreamer(api)
	s.setTool("WebSearch")
	time.Sleep(60 * time.Millisecond)
	api.mu.Lock()
	during := api.edits["900000000000000001"]
	api.mu.Unlock()
	if !strings.Contains(during, "Searching the web") {
		t.Fatalf("while a search runs the placeholder should say so, got %q", during)
	}
	s.setTool("")
	s.append("Here is the answer.")
	time.Sleep(60 * time.Millisecond) // several progress ticks pass
	s.close()
	if got := api.edits["900000000000000001"]; got != "Here is the answer." {
		t.Fatalf("a progress tick overwrote the streamed reply: %q", got)
	}
}

func TestStickerOnlyMessageReachesTheAgent(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(png) }))
	defer srv.Close()
	old := stickerURL
	stickerURL = func(id, ext string) string { return srv.URL + "/" + id + "." + ext }
	defer func() { stickerURL = old }()

	h := newHarness(t, "cute!")
	m := msg("600000000000000040", dmChan, linkedUser, "")
	m.StickerItems = []*discordgo.StickerItem{{ID: "700000000000000001", Name: "wave", FormatType: discordgo.StickerFormatTypePNG}}
	h.bot.handler.HandleMessage(context.Background(), m)

	if h.agent.turnCount() != 1 {
		t.Fatal("a sticker-only message must start a turn, not vanish")
	}
	req := h.agent.turns[0]
	if !strings.Contains(req.Text, "(sticker from The Owner: wave)") {
		t.Fatalf("turn text %q should name the sender and sticker", req.Text)
	}
	if len(req.Images) != 1 {
		t.Fatalf("want the sticker image attached, got %d images", len(req.Images))
	}
}

func TestLottieStickerIsDescribedInWords(t *testing.T) {
	h := newHarness(t, "ok")
	m := msg("600000000000000041", dmChan, linkedUser, "")
	m.StickerItems = []*discordgo.StickerItem{{ID: "700000000000000002", Name: "dance", FormatType: discordgo.StickerFormatTypeLottie}}
	h.bot.handler.HandleMessage(context.Background(), m)
	if h.agent.turnCount() != 1 || len(h.agent.turns[0].Images) != 0 {
		t.Fatal("a Lottie sticker has no image form: words only")
	}
	if !strings.Contains(h.agent.turns[0].Text, "dance") {
		t.Fatalf("turn text %q lost the sticker name", h.agent.turns[0].Text)
	}
}

func TestReactionActionGetsAResultMark(t *testing.T) {
	h := newHarness(t, "an answer")
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000050", dmChan, linkedUser, "hi"))
	reply := h.api.sends[0].id
	h.api.reacts = nil
	h.bot.handler.HandleReaction(context.Background(), &discordgo.MessageReaction{
		UserID: linkedUser, MessageID: reply, ChannelID: dmChan, Emoji: discordgo.Emoji{Name: "❌"},
	})
	if got := strings.Join(h.api.reacts, " "); got != "+✅" {
		t.Fatalf("result marks = %q, want ✅ on the reacted reply", got)
	}
}

func TestNewServerChannelJoinsTheFamilyChat(t *testing.T) {
	h := newHarness(t, "hello, new channel")
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000060", "200000000000000013", linkedUser, "first message here"))
	if h.agent.turnCount() != 1 {
		t.Fatal("no turn")
	}
	if req := h.agent.turns[0]; req.ChatID != -100200300 {
		t.Fatalf("a channel created in the family server ran in chat %d, want the family chat", req.ChatID)
	}
	if h.api.sends[0].channel != "200000000000000013" {
		t.Fatalf("reply went to %s, want the new channel", h.api.sends[0].channel)
	}
}

func TestReminderMentionPingsOnlyThatPerson(t *testing.T) {
	h := newHarness(t, "")
	h.bot.SendText(-100200300, 0, "@owner reminder: dinner at 6")
	got := h.api.sends[len(h.api.sends)-1]
	if got.content != "<@"+linkedUser+"> reminder: dinner at 6" {
		t.Fatalf("content = %q", got.content)
	}
	if len(got.pings) != 1 || got.pings[0] != linkedUser {
		t.Fatalf("pings = %v, want only the named person", got.pings)
	}
	h.bot.SendText(-100200300, 0, "a reminder for nobody in particular")
	if p := h.api.sends[len(h.api.sends)-1].pings; len(p) != 0 {
		t.Fatalf("an unnamed reminder must ping no one, got %v", p)
	}
}

func TestReplyQuotesTheMessageRepliedTo(t *testing.T) {
	h := newHarness(t, "ok")
	m := msg("600000000000000070", dmChan, linkedUser, "yes, that one")
	m.ReferencedMessage = &discordgo.Message{ID: "600000000000000069", Content: "Option A: the ramen place\nOption B: tacos",
		Author: &discordgo.User{ID: selfBot, Bot: true}}
	h.bot.handler.HandleMessage(context.Background(), m)
	text := h.agent.turns[0].Text
	if !strings.Contains(text, `[Replying to your earlier message: "Option A: the ramen place Option B: tacos"]`) {
		t.Fatalf("turn text lacks the quote:\n%s", text)
	}
	if !strings.Contains(text, "yes, that one") {
		t.Fatal("the reply itself was lost")
	}

	h2 := newHarness(t, "ok")
	m2 := msg("600000000000000071", dmChan, linkedUser, "/help")
	m2.ReferencedMessage = &discordgo.Message{ID: "600000000000000069", Content: "x", Author: &discordgo.User{ID: selfBot}}
	h2.bot.handler.HandleMessage(context.Background(), m2)
	if h2.agent.turnCount() != 0 {
		t.Fatal("a command sent as a reply must still run as a command")
	}
}
