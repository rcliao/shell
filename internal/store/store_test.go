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

	err := s.SaveSession(12345, "claude-sess-abc")
	if err != nil {
		t.Fatalf("save session: %v", err)
	}

	sess, err := s.GetSession(12345)
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

	sess, err := s.GetSession(99999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess != nil {
		t.Error("expected nil session")
	}
}

func TestSaveSession_Upsert(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, "sess-1")
	s.SaveSession(100, "sess-2")

	sess, err := s.GetSession(100)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ProviderSessionID != "sess-2" {
		t.Errorf("expected sess-2 after upsert, got %s", sess.ProviderSessionID)
	}
}

func TestLogAndGetMessages(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)

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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)
	s.LogMessage(sess.ID, "user", "hello")

	err := s.DeleteSession(100)
	if err != nil {
		t.Fatal(err)
	}

	sess, err = s.GetSession(100)
	if err != nil {
		t.Fatal(err)
	}
	if sess != nil {
		t.Error("expected nil after delete")
	}
}

func TestListActiveSessions(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, "sess-1")
	s.SaveSession(200, "sess-2")

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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)

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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)
	s.SaveMessageMap(100, 10, 20, sess.ID, "hello", "hi")

	err := s.DeleteSession(100)
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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)

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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)
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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)
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

	s.SaveSession(100, "sess-1")
	sess, _ := s.GetSession(100)

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

func TestStaleSessionChatIDs(t *testing.T) {
	s := testStore(t)

	s.SaveSession(100, "sess-1")

	// With a very short idle duration, nothing should be stale yet
	ids, err := s.StaleSessionChatIDs(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 stale sessions, got %d", len(ids))
	}
}
