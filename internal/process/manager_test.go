package process

import (
	"strings"
	"testing"
)

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

func TestFormatMessage_TextOnly(t *testing.T) {
	msg := FormatMessage(AgentRequest{Text: "hello world"})
	if msg != "hello world" {
		t.Errorf("expected 'hello world', got %q", msg)
	}
}

func TestFormatMessage_WithImages(t *testing.T) {
	msg := FormatMessage(AgentRequest{
		Text: "what is this?",
		Images: []ImageAttachment{
			{Path: "/tmp/photo.jpg", Width: 800, Height: 600, Size: 50000},
		},
	})
	if !strings.Contains(msg, "[Attached image: /tmp/photo.jpg") {
		t.Errorf("expected image metadata, got %q", msg)
	}
	if !strings.Contains(msg, "800x600") {
		t.Errorf("expected dimensions, got %q", msg)
	}
	if !strings.Contains(msg, "48.8 KB") {
		t.Errorf("expected size, got %q", msg)
	}
	if !strings.HasSuffix(msg, "what is this?") {
		t.Errorf("expected text at end, got %q", msg)
	}
}

func TestFormatMessage_WithPDFs(t *testing.T) {
	msg := FormatMessage(AgentRequest{
		Text: "summarize this",
		PDFs: []PDFAttachment{
			{Path: "/tmp/doc.pdf", Size: 1048576},
		},
	})
	if !strings.Contains(msg, "[Attached PDF: /tmp/doc.pdf") {
		t.Errorf("expected PDF metadata, got %q", msg)
	}
	if !strings.Contains(msg, "1.0 MB") {
		t.Errorf("expected size, got %q", msg)
	}
}

func TestFormatMessage_NoSizeOrDimensions(t *testing.T) {
	msg := FormatMessage(AgentRequest{
		Text:   "check this",
		Images: []ImageAttachment{{Path: "/tmp/x.png"}},
	})
	if msg != "[Attached image: /tmp/x.png]\ncheck this" {
		t.Errorf("unexpected format: %q", msg)
	}
}

func TestFormatFileSize(t *testing.T) {
	tests := []struct {
		size int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1048576, "1.0 MB"},
	}
	for _, tt := range tests {
		if got := formatFileSize(tt.size); got != tt.want {
			t.Errorf("formatFileSize(%d) = %q, want %q", tt.size, got, tt.want)
		}
	}
}

func TestFilterEnv(t *testing.T) {
	env := []string{"PATH=/usr/bin", "CLAUDECODE=abc", "HOME=/home/user"}
	filtered := filterEnv(env, "CLAUDECODE")
	if len(filtered) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(filtered))
	}
	for _, e := range filtered {
		if strings.HasPrefix(e, "CLAUDECODE=") {
			t.Error("CLAUDECODE should have been removed")
		}
	}
}
