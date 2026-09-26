package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

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
		runTurn: func(_ context.Context, p string) (string, error) {
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
		runTurn: func(context.Context, string) (string, error) { ran = true; return "", nil },
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
