package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

func TestChatRetroGroupsByLaneAndPostsOne(t *testing.T) {
	st := openReviewStore(t)
	const chat = int64(-100200300)
	st.SaveSession(chat, 0, "c")
	sess, _ := st.GetSession(chat, 0)
	for _, m := range []string{"lunch memo: noodles", "is the museum open sunday?", "dinner memo: rice"} {
		st.LogMessage(sess.ID, "user", m)
	}
	st.LogRouteDecision(store.RouteDecision{Source: "lane", ChatID: chat, MsgAt: time.Now(), TextHash: store.TextHash("lunch memo: noodles"), Backend: "jev-v2", Lane: "health"})
	st.LogRouteDecision(store.RouteDecision{Source: "lane", ChatID: chat, MsgAt: time.Now(), TextHash: store.TextHash("dinner memo: rice"), Backend: "jev-v2", Lane: "health"})

	var prompt string
	var posted []string
	d := chatRetroDeps{store: st, agentName: "a", chats: []int64{chat},
		runTurn: func(_ context.Context, _ int64, p string) (string, error) {
			prompt = p
			st.CreateSuggestion(store.Suggestion{Title: "early", Change: "x", Audience: store.ChatAudience(chat)})
			st.CreateSuggestion(store.Suggestion{Title: "要不要…", Change: "回覆 好 或 不用", Audience: store.ChatAudience(chat)})
			st.CreateSuggestion(store.Suggestion{Title: "owner idea", Change: "y"}) // owner-addressed: not posted here
			return "filed one", nil
		},
		notify: func(chatID int64, text string) error { posted = append(posted, text); return nil },
	}
	res, err := d.handle(context.Background(), scheduler.LeasedTask{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Lane: health — 2 messages", "Lane: general — 1 messages", "for_chat=-100200300", "the family group", "(none yet)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if len(posted) != 1 || !strings.Contains(posted[0], "要不要…") || !strings.Contains(res, "posted") {
		t.Fatalf("posted %v (res %q), want only the newest chat suggestion", posted, res)
	}
	if w, _ := st.ListSuggestions([]string{store.SuggestionWithdrawn}, 0); len(w) != 1 || w[0].Title != "early" {
		t.Errorf("extra chat suggestion must be withdrawn: %+v", w)
	}
	if p, _ := st.ProposedFor("owner"); len(p) != 1 {
		t.Errorf("the owner suggestion stays for the owner review: %+v", p)
	}
	if open, _ := st.OpenChatSuggestions(chat, time.Hour); len(open) != 1 {
		t.Errorf("the posted suggestion is open in the chat: %+v", open)
	}
}

func TestChatRetroSkipsQuietChats(t *testing.T) {
	st := openReviewStore(t)
	ran := false
	d := chatRetroDeps{store: st, chats: []int64{42},
		runTurn: func(context.Context, int64, string) (string, error) { ran = true; return "", nil },
		notify:  func(int64, string) error { return nil }}
	if res, err := d.handle(context.Background(), scheduler.LeasedTask{}); err != nil || ran || !strings.Contains(res, "no messages") {
		t.Fatalf("res=%q err=%v ran=%v", res, err, ran)
	}
}

// The owner review must never deliver a chat's suggestion to the owner.
func TestOwnerReviewIgnoresChatSuggestions(t *testing.T) {
	st := openReviewStore(t)
	st.CreateSuggestion(store.Suggestion{Title: "for the family", Change: "x", Audience: store.ChatAudience(-1)})
	var sent string
	d := reviewDeps{store: st, agentName: "a", ownerChatID: 42,
		runTurn: func(context.Context, string) (string, error) { return "summary", nil },
		notify:  func(_ int64, text string) error { sent = text; return nil }}
	d.handle(context.Background(), scheduler.LeasedTask{})
	if strings.Contains(sent, "for the family") {
		t.Errorf("owner DM got a chat suggestion: %q", sent)
	}
}

// Chat 1 posts, chat 2 finds the session busy: the task must NOT fail (a
// retry would re-run chat 1 and post into it twice).
func TestChatRetroBusyAfterAPostDoesNotReplay(t *testing.T) {
	st := openReviewStore(t)
	for _, c := range []int64{-1, -2} {
		st.SaveSession(c, 0, "c")
		ss, _ := st.GetSession(c, 0)
		st.LogMessage(ss.ID, "user", "hello")
	}
	calls := 0
	d := chatRetroDeps{store: st, chats: []int64{-1, -2},
		runTurn: func(context.Context, int64, string) (string, error) {
			calls++
			if calls == 2 {
				return "", fmt.Errorf("wrapped: %w", process.ErrSessionBusy)
			}
			st.CreateSuggestion(store.Suggestion{Title: "t", Change: "c", Audience: store.ChatAudience(-1)})
			return "", nil
		},
		notify: func(int64, string) error { return nil }}
	res, err := d.handle(context.Background(), scheduler.LeasedTask{})
	if err != nil || !strings.Contains(res, "skipped this week") {
		t.Fatalf("res=%q err=%v: a busy second chat must not fail the task", res, err)
	}
}

// A re-run of the task (queue retry, manual trigger) posts nothing twice, and
// a suggestion filed for another chat is never posted here.
func TestChatRetroIdempotentAndOwnTurnOnly(t *testing.T) {
	st := openReviewStore(t)
	st.SaveSession(-1, 0, "c")
	ss, _ := st.GetSession(-1, 0)
	st.LogMessage(ss.ID, "user", "hello")
	stray, _ := st.CreateSuggestion(store.Suggestion{Title: "stray from another turn", Change: "x", Audience: store.ChatAudience(-1)})
	time.Sleep(1100 * time.Millisecond) // the stray predates this run's turn
	posts := 0
	d := chatRetroDeps{store: st, chats: []int64{-1},
		runTurn: func(_ context.Context, chatID int64, _ string) (string, error) {
			if chatRetroThread(chatID) <= 0 {
				t.Error("retro sessions must be positive, never a lane")
			}
			st.CreateSuggestion(store.Suggestion{Title: "mine", Change: "c", Audience: store.ChatAudience(-1)})
			return "", nil
		},
		notify: func(int64, string) error { posts++; return nil }}
	d.handle(context.Background(), scheduler.LeasedTask{})
	d.handle(context.Background(), scheduler.LeasedTask{}) // re-run
	if posts != 1 {
		t.Fatalf("posted %d times, want exactly once", posts)
	}
	if s, _ := st.GetSuggestion(stray); s.Status != store.SuggestionWithdrawn {
		t.Errorf("a stray filed outside this chat's turn must be withdrawn, got %s", s.Status)
	}
	if chatRetroThread(-100200300) == chatRetroThread(-100200301) {
		t.Error("distinct chats need distinct retro sessions")
	}
}
