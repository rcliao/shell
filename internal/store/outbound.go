package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Outbound dedup ledger (V2-H3).
//
// Every proactive send (scheduler notify, relay, a2a reply, prompt-schedule
// result — anything that flows through Bot.SendText rather than a
// conversational reply edit) is recorded here by normalized-text hash. A send
// whose hash matches a non-suppressed send to the same chat within the dedup
// window is a duplicate: the caller suppresses it and records the row with
// suppressed=1 so the miss is measurable. Deterministic guard for the
// duplicate-reminder class (owner complaints 7/3 and 7/10).

// outboundDedupMinRunes guards very short texts ("✅ Done") from false-positive
// suppression — two unrelated proactive sends can legitimately share a short
// acknowledgement within the window. Reminders are always longer.
const outboundDedupMinRunes = 16

// NormalizeOutbound collapses runs of whitespace to single spaces and trims,
// so cosmetic formatting differences don't defeat the hash.
func NormalizeOutbound(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// OutboundTextHash returns the dedup key for an outbound text: sha256 hex of
// the normalized text. Returns "" when the text is too short to dedup safely.
func OutboundTextHash(text string) string {
	norm := NormalizeOutbound(text)
	if len([]rune(norm)) < outboundDedupMinRunes {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// RecordOutboundSend appends a ledger row for a proactive send (or a
// suppressed duplicate of one).
func (s *Store) RecordOutboundSend(chatID, threadID int64, source, textHash string, suppressed bool) error {
	_, err := s.db.Exec(`
		INSERT INTO outbound_sends (chat_id, thread_id, source, text_hash, suppressed)
		VALUES (?, ?, ?, ?, ?)`,
		chatID, threadID, source, textHash, boolToInt(suppressed))
	return err
}

// SeenOutboundSince reports whether a non-suppressed send with the same hash
// went to the same chat/thread at or after the cutoff.
func (s *Store) SeenOutboundSince(chatID, threadID int64, textHash string, since time.Time) (bool, error) {
	var n int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM outbound_sends
		WHERE chat_id = ? AND thread_id = ? AND text_hash = ? AND suppressed = 0
		  AND sent_at >= ?`,
		chatID, threadID, textHash, since.UTC()).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
