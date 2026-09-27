package daemon

import (
	"context"
	"testing"
	"time"
)

// recordingOutbound counts sends; everything else is headless.
type recordingOutbound struct {
	headlessOutbound
	texts   []int64
	dedup   bool
	edits   []int
	discord map[int64]bool
}

func (r *recordingOutbound) SendText(chatID, threadID int64, text string) {
	r.texts = append(r.texts, chatID)
}
func (r *recordingOutbound) SetOutboundDedup(func(chatID, threadID int64, text string) bool) {
	r.dedup = true
}
func (r *recordingOutbound) Routes(chatID, threadID int64) bool { return r.discord[chatID] }
func (r *recordingOutbound) EditMessage(chatID int64, messageID int, text string) error {
	r.edits = append(r.edits, messageID)
	return nil
}

func TestRoutedOutboundSendsEachChatOnItsPlatform(t *testing.T) {
	tg := &recordingOutbound{}
	dc := &recordingOutbound{discord: map[int64]bool{-100200300: true}}
	r := routedOutbound{telegram: tg, discord: dc}

	r.SendText(-100200300, 0, "linked group → discord")
	r.SendText(42, 0, "unlinked dm → telegram")
	r.SendText(0, 0, "system chat → telegram")

	if len(dc.texts) != 1 || dc.texts[0] != -100200300 {
		t.Fatalf("discord got %v", dc.texts)
	}
	if len(tg.texts) != 2 {
		t.Fatalf("telegram got %v", tg.texts)
	}

	r.SetOutboundDedup(func(int64, int64, string) bool { return false })
	if !tg.dedup || !dc.dedup {
		t.Fatal("dedup must reach both transports")
	}
}

func TestRoutedOutboundStartReturnsWhenBothStop(t *testing.T) {
	r := routedOutbound{telegram: headlessOutbound{}, discord: &recordingOutbound{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Start(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after cancel")
	}
}

func TestRoutedOutboundEditsFollowTheMessageID(t *testing.T) {
	// An edit names only chat + message; the id says which platform sent it.
	tg := &recordingOutbound{}
	dc := &recordingOutbound{}
	r := routedOutbound{telegram: tg, discord: dc}
	r.EditMessage(-100200300, 4812, "telegram message")               // Telegram numbers from 1
	r.EditMessage(-100200300, 400000000000000001, "discord message") // snowflake
	if len(tg.edits) != 1 || tg.edits[0] != 4812 || len(dc.edits) != 1 {
		t.Fatalf("telegram edits %v, discord edits %v", tg.edits, dc.edits)
	}
}
