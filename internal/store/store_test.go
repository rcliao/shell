package store

import (
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveAndGetSession(t *testing.T) {
	s := testStore(t)

	err := s.SaveSession(12345, 0, "claude-sess-abc")
	if err != nil {
		t.Fatalf("save session: %v", err)
	}

	sess, err := s.GetSession(12345, 0)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if sess.ChatID != 12345 {
		t.Errorf("expected chat_id 12345, got %d", sess.ChatID)
	}
	if sess.ProviderSessionID != "claude-sess-abc" {
		t.Errorf("expected claude-sess-abc, got %s", sess.ProviderSessionID)
	}
}

func TestGetSession_NotFound(t *testing.T) {
	s := testStore(t)

	sess, err := s.GetSession(99999, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess != nil {
		t.Error("expected nil session")
	}
}

func TestSaveSession_Upsert(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	s.SaveSession(100, 0, "sess-2")

	sess, err := s.GetSession(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ProviderSessionID != "sess-2" {
		t.Errorf("expected sess-2 after upsert, got %s", sess.ProviderSessionID)
	}
}

func TestLogAndGetMessages(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)

	s.LogMessage(sess.ID, "user", "hello")
	s.LogMessage(sess.ID, "assistant", "hi there")
	s.LogMessage(sess.ID, "user", "how are you")

	msgs, err := s.GetMessages(sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Content != "hello" {
		t.Errorf("unexpected first message: %+v", msgs[0])
	}
	if msgs[2].Role != "user" || msgs[2].Content != "how are you" {
		t.Errorf("unexpected last message: %+v", msgs[2])
	}
}

func TestDeleteSession(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)
	s.LogMessage(sess.ID, "user", "hello")

	err := s.DeleteSession(100, 0)
	if err != nil {
		t.Fatal(err)
	}

	sess, err = s.GetSession(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sess != nil {
		t.Error("expected nil after delete")
	}
}

func TestListActiveSessions(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	s.SaveSession(200, 0, "sess-2")

	sessions, err := s.ListActiveSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestSaveAndGetMessageMap(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)

	err := s.SaveMessageMap(100, 10, 20, sess.ID, "hello world", "hi there")
	if err != nil {
		t.Fatalf("save message map: %v", err)
	}

	m, err := s.GetMessageMapByBotMsg(100, 20)
	if err != nil {
		t.Fatalf("get message map: %v", err)
	}
	if m == nil {
		t.Fatal("expected message map, got nil")
	}
	if m.ChatID != 100 {
		t.Errorf("expected chat_id 100, got %d", m.ChatID)
	}
	if m.UserMessageID != 10 {
		t.Errorf("expected user_message_id 10, got %d", m.UserMessageID)
	}
	if m.BotMessageID != 20 {
		t.Errorf("expected bot_message_id 20, got %d", m.BotMessageID)
	}
	if m.SessionID != sess.ID {
		t.Errorf("expected session_id %d, got %d", sess.ID, m.SessionID)
	}
	if m.UserMessage != "hello world" {
		t.Errorf("expected user_message 'hello world', got %q", m.UserMessage)
	}
	if m.BotResponse != "hi there" {
		t.Errorf("expected bot_response 'hi there', got %q", m.BotResponse)
	}
}

func TestGetMessageMap_NotFound(t *testing.T) {
	s := testStore(t)

	m, err := s.GetMessageMapByBotMsg(100, 999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m != nil {
		t.Error("expected nil message map")
	}
}

func TestDeleteSession_CascadesMessageMap(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)
	s.SaveMessageMap(100, 10, 20, sess.ID, "hello", "hi")

	err := s.DeleteSession(100, -1)
	if err != nil {
		t.Fatal(err)
	}

	m, err := s.GetMessageMapByBotMsg(100, 20)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Error("expected nil after session delete")
	}
}

func TestSaveMessageMap_MultipleChunks(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)

	// Simulate chunked response: multiple bot messages for one user message
	s.SaveMessageMap(100, 10, 20, sess.ID, "hello", "response part 1")
	s.SaveMessageMap(100, 10, 21, sess.ID, "hello", "response part 1")
	s.SaveMessageMap(100, 10, 22, sess.ID, "hello", "response part 1")

	for _, botID := range []int{20, 21, 22} {
		m, err := s.GetMessageMapByBotMsg(100, botID)
		if err != nil {
			t.Fatalf("get message map for bot_id %d: %v", botID, err)
		}
		if m == nil {
			t.Fatalf("expected message map for bot_id %d, got nil", botID)
		}
		if m.UserMessageID != 10 {
			t.Errorf("bot_id %d: expected user_message_id 10, got %d", botID, m.UserMessageID)
		}
	}
}

func TestDeleteMessageMap(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)
	s.SaveMessageMap(100, 10, 20, sess.ID, "hello", "hi")

	m, _ := s.GetMessageMapByBotMsg(100, 20)
	if m == nil {
		t.Fatal("expected message map")
	}

	err := s.DeleteMessageMap(m.ID)
	if err != nil {
		t.Fatalf("delete message map: %v", err)
	}

	m, err = s.GetMessageMapByBotMsg(100, 20)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		t.Error("expected nil after delete")
	}
}

func TestUpdateMessageMapResponse(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)
	s.SaveMessageMap(100, 10, 20, sess.ID, "question", "old answer")

	m, _ := s.GetMessageMapByBotMsg(100, 20)
	if m == nil {
		t.Fatal("expected message map")
	}

	err := s.UpdateMessageMapResponse(m.ID, "new answer")
	if err != nil {
		t.Fatalf("update message map response: %v", err)
	}

	m, err = s.GetMessageMapByBotMsg(100, 20)
	if err != nil {
		t.Fatal(err)
	}
	if m.BotResponse != "new answer" {
		t.Errorf("expected 'new answer', got %q", m.BotResponse)
	}
	// User message should remain unchanged.
	if m.UserMessage != "question" {
		t.Errorf("expected 'question', got %q", m.UserMessage)
	}
}

func TestDeleteExchangeMessages(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")
	sess, _ := s.GetSession(100, 0)

	s.LogMessage(sess.ID, "user", "first question")
	s.LogMessage(sess.ID, "assistant", "first answer")
	s.LogMessage(sess.ID, "user", "second question")
	s.LogMessage(sess.ID, "assistant", "second answer")

	err := s.DeleteExchangeMessages(sess.ID, "second question", "second answer")
	if err != nil {
		t.Fatalf("delete exchange: %v", err)
	}

	msgs, err := s.GetMessages(sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages after delete, got %d", len(msgs))
	}
	if msgs[0].Content != "first question" || msgs[1].Content != "first answer" {
		t.Errorf("unexpected remaining messages: %+v", msgs)
	}
}

func TestStaleSessionRefs(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, 0, "sess-1")

	// With a very short idle duration, nothing should be stale yet
	refs, err := s.StaleSessionRefs(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 stale sessions, got %d", len(refs))
	}
}

func TestSession_LifecycleFields_DefaultsOnFreshRow(t *testing.T) {
	s := testStore(t)

	if err := s.SaveSession(100, 0, "sess-1"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("expected session")
	}
	if sess.Generation != 1 {
		t.Errorf("expected generation=1 on fresh row, got %d", sess.Generation)
	}
	if sess.PrefixHash != "" {
		t.Errorf("expected empty prefix_hash on fresh row, got %q", sess.PrefixHash)
	}
	if sess.RotatePending {
		t.Error("expected rotate_pending=false on fresh row")
	}
	if sess.CompactState != "" {
		t.Errorf("expected empty compact_state on fresh row, got %q", sess.CompactState)
	}
	if sess.GenerationStartedAt.IsZero() {
		t.Error("expected generation_started_at to be set on fresh row")
	}
}

func TestSession_SaveSessionPreservesLifecycleFields(t *testing.T) {
	s := testStore(t)

	if err := s.SaveSession(100, 0, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrefixHash(100, 0, "hash-abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRotatePending(100, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCompactState(100, 0, "compacting"); err != nil {
		t.Fatal(err)
	}

	// Re-saving (simulating a normal turn write-back) must NOT clobber
	// lifecycle fields.
	if err := s.SaveSession(100, 0, "sess-2"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ProviderSessionID != "sess-2" {
		t.Errorf("expected UUID advance to sess-2, got %s", sess.ProviderSessionID)
	}
	if sess.PrefixHash != "hash-abc" {
		t.Errorf("prefix_hash clobbered: got %q", sess.PrefixHash)
	}
	if !sess.RotatePending {
		t.Error("rotate_pending clobbered")
	}
	if sess.CompactState != "compacting" {
		t.Errorf("compact_state clobbered: got %q", sess.CompactState)
	}
}

func TestSession_BumpGeneration(t *testing.T) {
	s := testStore(t)
	s.SaveSession(100, 0, "sess-1")
	s.SetPrefixHash(100, 0, "old-hash")
	s.SetRotatePending(100, 0, true)
	s.SetCompactState(100, 0, "compacting")

	newGen, err := s.BumpGeneration(100, 0, "new-hash")
	if err != nil {
		t.Fatal(err)
	}
	if newGen != 2 {
		t.Errorf("expected generation 2, got %d", newGen)
	}

	sess, _ := s.GetSession(100, 0)
	if sess.Generation != 2 {
		t.Errorf("generation not persisted: got %d", sess.Generation)
	}
	if sess.PrefixHash != "new-hash" {
		t.Errorf("prefix_hash not updated: got %q", sess.PrefixHash)
	}
	if sess.ProviderSessionID != "" {
		t.Errorf("expected claude_session_id cleared on rotation, got %q", sess.ProviderSessionID)
	}
	if sess.RotatePending {
		t.Error("expected rotate_pending cleared on rotation")
	}
	if sess.CompactState != "" {
		t.Error("expected compact_state cleared on rotation")
	}
}

func TestSession_SaveAndGetSessionSummary(t *testing.T) {
	s := testStore(t)
	s.SaveSession(100, 0, "sess-1")

	if err := s.SaveSessionSummary(100, 0, 1, "prior convo: lunch talk", `{"memories":["prefer Asian"]}`); err != nil {
		t.Fatal(err)
	}
	sm, err := s.GetLatestSessionSummary(100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if sm == nil {
		t.Fatal("expected summary")
	}
	if sm.Generation != 1 {
		t.Errorf("expected generation 1, got %d", sm.Generation)
	}
	if sm.Summary != "prior convo: lunch talk" {
		t.Errorf("summary round-trip mismatch: %q", sm.Summary)
	}
	if sm.MemoryPack != `{"memories":["prefer Asian"]}` {
		t.Errorf("memory_pack round-trip mismatch: %q", sm.MemoryPack)
	}

	// Writing generation 2 → GetLatest should return it.
	s.SaveSessionSummary(100, 0, 2, "later convo", "")
	sm, _ = s.GetLatestSessionSummary(100, 0)
	if sm.Generation != 2 {
		t.Errorf("expected latest generation 2, got %d", sm.Generation)
	}
}

func TestSession_DeleteSessionCascadesSummaries(t *testing.T) {
	s := testStore(t)
	s.SaveSession(100, 0, "sess-1")
	s.SaveSessionSummary(100, 0, 1, "summary", "")

	if err := s.DeleteSession(100, 0); err != nil {
		t.Fatal(err)
	}
	sm, _ := s.GetLatestSessionSummary(100, 0)
	if sm != nil {
		t.Error("expected session summary to be deleted with session")
	}
}

func TestSession_ThreadIsolation(t *testing.T) {
	s := testStore(t)

	// Two topics in the same chat → two distinct sessions.
	if err := s.SaveSession(100, 0, "sess-main"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(100, 42, "sess-topic42"); err != nil {
		t.Fatal(err)
	}

	main, _ := s.GetSession(100, 0)
	topic, _ := s.GetSession(100, 42)
	if main == nil || topic == nil {
		t.Fatal("expected both sessions to exist")
	}
	if main.ProviderSessionID == topic.ProviderSessionID {
		t.Error("main and topic sessions should have distinct provider session IDs")
	}
	if main.MessageThreadID != 0 || topic.MessageThreadID != 42 {
		t.Errorf("thread ids wrong: main=%d topic=%d", main.MessageThreadID, topic.MessageThreadID)
	}

	// Delete just the topic session — main must remain.
	if err := s.DeleteSession(100, 42); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSession(100, 42); got != nil {
		t.Error("expected topic session to be deleted")
	}
	if got, _ := s.GetSession(100, 0); got == nil {
		t.Error("main session was erroneously deleted")
	}
}
