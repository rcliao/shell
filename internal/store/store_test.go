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
	if sess.ClaudeSessionID != "claude-sess-abc" {
		t.Errorf("expected claude-sess-abc, got %s", sess.ClaudeSessionID)
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
	if sess.ClaudeSessionID != "sess-2" {
		t.Errorf("expected sess-2 after upsert, got %s", sess.ClaudeSessionID)
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
