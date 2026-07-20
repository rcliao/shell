package process

import "testing"

// KillChat must reap EVERY session of a chat — main thread and topic threads
// alike — and report the count. The 7/20 stale-OAuth recovery failed because
// the kill path touched only bookkeeping, so an operator "killed" sessions
// while the real subprocesses kept answering with a dead token.
func TestKillChatReapsAllThreadsOfChat(t *testing.T) {
	m := &Manager{
		sessions:   make(map[SessionKey]*Session),
		persistent: make(map[SessionKey]*persistentProc),
	}
	target := int64(-100)
	other := int64(42)
	for _, k := range []SessionKey{
		{ChatID: target, ThreadID: 0},
		{ChatID: target, ThreadID: 1419},
		{ChatID: target, ThreadID: 2479},
		{ChatID: other, ThreadID: 0},
	} {
		m.sessions[k] = &Session{Status: StatusActive}
	}

	if got := m.KillChat(target); got != 3 {
		t.Fatalf("KillChat reaped %d sessions, want 3", got)
	}
	for k := range m.sessions {
		if k.ChatID == target {
			t.Fatalf("session %+v survived KillChat", k)
		}
	}
	if _, ok := m.sessions[SessionKey{ChatID: other, ThreadID: 0}]; !ok {
		t.Fatal("KillChat must not touch other chats")
	}
	if got := m.KillChat(target); got != 0 {
		t.Fatalf("second KillChat reaped %d, want 0 (idempotent)", got)
	}
}
