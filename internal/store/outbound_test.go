package store

import (
	"testing"
	"time"
)

func TestOutboundTextHash(t *testing.T) {
	long := "⚡💛 晚上9點到了！！記得用鼻噴喔～ 🌸"
	if OutboundTextHash(long) == "" {
		t.Fatal("reminder-length text must produce a hash")
	}
	// Whitespace-only differences must collide (same dedup key).
	if OutboundTextHash("hello   world foo bar baz quux") != OutboundTextHash("hello world\n foo  bar baz quux") {
		t.Fatal("normalization must collapse whitespace")
	}
	// Short acks are exempt from dedup.
	if OutboundTextHash("✅ Done") != "" {
		t.Fatal("short text must not be deduped")
	}
}

func TestOutboundDedupLedger(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	hash := OutboundTextHash("⏰ 6:00 PM reminder — take out the trash please!")
	cutoff := time.Now().Add(-60 * time.Minute)

	// Nothing recorded yet → not seen.
	seen, err := s.SeenOutboundSince(100, 0, hash, cutoff)
	if err != nil || seen {
		t.Fatalf("seen=%v err=%v, want unseen", seen, err)
	}

	// First send recorded → duplicate now seen in-window.
	if err := s.RecordOutboundSend(100, 0, "sendtext", hash, false); err != nil {
		t.Fatal(err)
	}
	seen, err = s.SeenOutboundSince(100, 0, hash, cutoff)
	if err != nil || !seen {
		t.Fatalf("seen=%v err=%v, want seen", seen, err)
	}

	// Different chat, different thread, different text → all unseen.
	for _, tc := range []struct {
		chat, thread int64
		h            string
	}{
		{200, 0, hash},
		{100, 7, hash},
		{100, 0, OutboundTextHash("a completely different reminder message here")},
	} {
		seen, err = s.SeenOutboundSince(tc.chat, tc.thread, tc.h, cutoff)
		if err != nil || seen {
			t.Fatalf("chat=%d thread=%d: seen=%v err=%v, want unseen", tc.chat, tc.thread, seen, err)
		}
	}

	// Suppressed rows must NOT count as prior sends (they never reached the
	// user), but must be recorded for measurement.
	if err := s.RecordOutboundSend(300, 0, "sendtext", hash, true); err != nil {
		t.Fatal(err)
	}
	seen, err = s.SeenOutboundSince(300, 0, hash, cutoff)
	if err != nil || seen {
		t.Fatalf("seen=%v err=%v, suppressed row must not ground dedup", seen, err)
	}

	// A send outside the window must not match.
	if _, err := s.db.Exec(`UPDATE outbound_sends SET sent_at = ? WHERE chat_id = 100`,
		time.Now().Add(-2*time.Hour).UTC()); err != nil {
		t.Fatal(err)
	}
	seen, err = s.SeenOutboundSince(100, 0, hash, cutoff)
	if err != nil || seen {
		t.Fatalf("seen=%v err=%v, out-of-window send must not match", seen, err)
	}
}
