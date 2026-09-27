package discord

import (
	"context"
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
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{channels: map[string]*discordgo.Channel{}, nextID: 400000000000000000, edits: map[string]string{}}
}

func (f *fakeAPI) Send(ch string, m *discordgo.MessageSend) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := strconv.FormatInt(f.nextID, 10)
	f.sends = append(f.sends, sent{channel: ch, id: id, content: m.Content, files: len(m.Files), buttons: len(m.Components)})
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
func (f *fakeAPI) React(ch, id, e string) error   { f.mu.Lock(); f.reacts = append(f.reacts, "+"+e); f.mu.Unlock(); return nil }
func (f *fakeAPI) Unreact(ch, id, e string) error { f.mu.Lock(); f.reacts = append(f.reacts, "-"+e); f.mu.Unlock(); return nil }
func (f *fakeAPI) Typing(string) error            { return nil }
func (f *fakeAPI) Pin(string, string) error       { return nil }
func (f *fakeAPI) Unpin(string, string) error     { return nil }
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
	br := bridge.New(agent, s, nil, nil, false, "", nil, nil, nil, nil)

	api := newFakeAPI()
	api.channels[dmChan] = &discordgo.Channel{ID: dmChan, Type: discordgo.ChannelTypeDM}
	api.channels[groupChan] = &discordgo.Channel{ID: groupChan, Type: discordgo.ChannelTypeGuildText}
	addr := NewAddresses(config.DiscordConfig{
		Users: map[string]int64{linkedUser: 42},
		Chats: map[string]config.DiscordChatLink{
			dmChan:    {ChatID: 42},
			groupChan: {ChatID: -100200300},
		},
	})
	b := newBot(api, Options{
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
	if len(h.api.sends) != 1 || h.api.sends[0].content != "Thinking…" || h.api.sends[0].channel != dmChan {
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
