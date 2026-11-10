package discord

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

var errTimeout = errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)")

// recordingLedger notes whether a turn was recorded answered or handed back.
type recordingLedger struct {
	mu                   sync.Mutex
	completed, abandoned int
}

func (l *recordingLedger) Begin(int64, int64, int, string, string) (bool, error) { return false, nil }
func (l *recordingLedger) Complete(int64, int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.completed++
	return nil
}
func (l *recordingLedger) Abandon(int64, int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.abandoned++
	return nil
}
func (l *recordingLedger) Undelivered(time.Duration) (int, error) { return 0, nil }

func deliveryHarness(t *testing.T, reply string) (*harness, *recordingLedger) {
	t.Helper()
	h := newHarness(t, reply)
	led := &recordingLedger{}
	h.br.SetTurnLedger(led)
	ms := []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	h.bot.handler.editRetry, h.bot.handler.sendRetry = ms, ms
	return h, led
}

// The final edit timing out once must not cost the reply: it is retried.
func TestFinalEditIsRetried(t *testing.T) {
	const reply = "Dinner is at seven."
	h, led := deliveryHarness(t, reply)
	failures := 2
	h.api.editErr = func(content string) error {
		if strings.Contains(content, reply) && failures > 0 {
			failures--
			return errTimeout
		}
		return nil
	}
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000101", dmChan, linkedUser, "when is dinner?"))

	placeholder := h.api.sends[0].id
	if got := h.api.edits[placeholder]; !strings.Contains(got, reply) {
		t.Fatalf("placeholder holds %q, want the reply", got)
	}
	if led.completed != 1 || led.abandoned != 0 {
		t.Fatalf("completed=%d abandoned=%d, want the turn recorded answered", led.completed, led.abandoned)
	}
}

// A placeholder that cannot be edited at all: the reply goes out as a new
// message. Before, the edit failure was only logged and the person was left
// looking at "Thinking…" while the turn was recorded answered.
func TestUneditablePlaceholderFallsBackToNewMessage(t *testing.T) {
	const reply = "Dinner is at seven."
	h, led := deliveryHarness(t, reply)
	h.api.editErr = func(content string) error {
		if strings.Contains(content, reply) {
			return errTimeout
		}
		return nil
	}
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000102", dmChan, linkedUser, "when is dinner?"))

	if len(h.api.sends) != 2 || !strings.Contains(h.api.sends[1].content, reply) {
		t.Fatalf("want the placeholder then the reply as a new message, got %+v", h.api.sends)
	}
	if led.completed != 1 || led.abandoned != 0 {
		t.Fatalf("completed=%d abandoned=%d, want the turn recorded answered", led.completed, led.abandoned)
	}
}

// The 2026-11-09 incident on the live path: nothing can be posted. The turn
// must go back to the queue for replay, not be recorded answered.
func TestUndeliverableReplyIsHandedBackNotCompleted(t *testing.T) {
	const reply = "Dinner is at seven."
	h, led := deliveryHarness(t, reply)
	h.api.editErr = func(content string) error {
		if strings.Contains(content, reply) {
			return errTimeout
		}
		return nil
	}
	h.api.sendErr = func(n int, _ *discordgo.MessageSend) error {
		if n >= 1 { // the placeholder got through; nothing after it does
			return errTimeout
		}
		return nil
	}
	h.bot.handler.HandleMessage(context.Background(), msg("600000000000000103", dmChan, linkedUser, "when is dinner?"))

	if led.completed != 0 {
		t.Fatal("an undelivered reply was recorded answered")
	}
	if led.abandoned != 1 {
		t.Fatalf("abandoned=%d, want the turn handed back for replay", led.abandoned)
	}
}

// SendReply ignores the outbound dedup. Dedup records a text before sending it,
// so with it a retry of a failed reply was judged a duplicate and reported
// delivered.
func TestSendReplyBypassesDedup(t *testing.T) {
	h := newHarness(t, "unused")
	h.bot.SetOutboundDedup(func(int64, int64, string) bool { return true })
	if err := h.bot.SendReply(-100200300, 0, "the answer"); err != nil {
		t.Fatal(err)
	}
	if len(h.api.sends) != 1 || h.api.sends[0].content != "the answer" {
		t.Fatalf("dedup swallowed the reply: %+v", h.api.sends)
	}
	h.api.sendErr = func(int, *discordgo.MessageSend) error { return errTimeout }
	if err := h.bot.SendReply(-100200300, 0, "the answer"); !errors.Is(err, errTimeout) {
		t.Fatalf("SendReply = %v, want the send error", err)
	}
}

// The REST client pings idle HTTP/2 connections so a dead one is replaced.
func TestRESTClientDetectsDeadConnections(t *testing.T) {
	c := restClient()
	if c.Timeout != 20*time.Second {
		t.Fatalf("timeout %s, want discordgo's 20s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatal("REST transport has no HTTP/2 health pings; a dead connection would block every send until restart")
	}
}
