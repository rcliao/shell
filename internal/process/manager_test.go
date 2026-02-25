package process

import "testing"

func TestNewManager(t *testing.T) {
	m := NewManager(ManagerConfig{
		Binary:      "echo",
		MaxSessions: 2,
	})
	if m.maxSessions != 2 {
		t.Errorf("expected maxSessions 2, got %d", m.maxSessions)
	}
	if m.binary != "echo" {
		t.Errorf("expected binary echo, got %s", m.binary)
	}
}

func TestNewManager_Defaults(t *testing.T) {
	m := NewManager(ManagerConfig{})
	if m.binary != "claude" {
		t.Errorf("expected default binary claude, got %s", m.binary)
	}
	if m.maxSessions != 4 {
		t.Errorf("expected default maxSessions 4, got %d", m.maxSessions)
	}
}

func TestRegisterAndGet(t *testing.T) {
	m := NewManager(ManagerConfig{Binary: "echo"})

	sess := NewSession(12345)
	m.Register(sess)

	got, ok := m.Get(12345)
	if !ok {
		t.Fatal("expected session to exist")
	}
	if got.ChatID != 12345 {
		t.Errorf("expected chat_id 12345, got %d", got.ChatID)
	}
}

func TestKillAndRemove(t *testing.T) {
	m := NewManager(ManagerConfig{Binary: "echo"})

	sess := NewSession(100)
	m.Register(sess)

	m.Kill(100)
	_, ok := m.Get(100)
	if ok {
		t.Error("expected session to be removed after kill")
	}
}

func TestKillAll(t *testing.T) {
	m := NewManager(ManagerConfig{Binary: "echo"})

	m.Register(NewSession(1))
	m.Register(NewSession(2))
	m.Register(NewSession(3))

	if m.ActiveCount() != 3 {
		t.Errorf("expected 3 active, got %d", m.ActiveCount())
	}

	m.KillAll()
	if m.ActiveCount() != 0 {
		t.Errorf("expected 0 active after killall, got %d", m.ActiveCount())
	}
}
