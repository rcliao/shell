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

func TestParseStreamEvents_StreamEvent(t *testing.T) {
	// Simulate the actual Claude CLI stream-json format with --verbose --include-partial-messages
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-123"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}}`,
		`{"type":"result","result":"Hello world!","session_id":"sess-123"}`,
	}, "\n")

	var deltas []string
	result := parseStreamEvents(strings.NewReader(input), func(delta string) {
		deltas = append(deltas, delta)
	})

	if result.Text != "Hello world!" {
		t.Errorf("expected result text 'Hello world!', got %q", result.Text)
	}
	if result.SessionID != "sess-123" {
		t.Errorf("expected session ID 'sess-123', got %q", result.SessionID)
	}
	if len(deltas) != 3 {
		t.Fatalf("expected 3 deltas, got %d", len(deltas))
	}
	if deltas[0] != "Hello" || deltas[1] != " world" || deltas[2] != "!" {
		t.Errorf("unexpected deltas: %v", deltas)
	}
}

func TestParseStreamEvents_AssistantFallback(t *testing.T) {
	// Test the assistant event type (older/alternative format)
	input := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Hello from assistant"}]},"session_id":"sess-456"}`,
		`{"type":"result","result":"Hello from assistant","session_id":"sess-456"}`,
	}, "\n")

	var deltas []string
	result := parseStreamEvents(strings.NewReader(input), func(delta string) {
		deltas = append(deltas, delta)
	})

	if result.Text != "Hello from assistant" {
		t.Errorf("expected result text, got %q", result.Text)
	}
	if result.SessionID != "sess-456" {
		t.Errorf("expected session ID 'sess-456', got %q", result.SessionID)
	}
	if len(deltas) != 1 || deltas[0] != "Hello from assistant" {
		t.Errorf("unexpected deltas: %v", deltas)
	}
}

func TestParseStreamEvents_SkipsUnparseable(t *testing.T) {
	input := strings.Join([]string{
		`not json at all`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}}`,
		``,
		`{"type":"result","result":"OK","session_id":"s1"}`,
	}, "\n")

	var deltas []string
	result := parseStreamEvents(strings.NewReader(input), func(delta string) {
		deltas = append(deltas, delta)
	})

	if result.Text != "OK" {
		t.Errorf("expected 'OK', got %q", result.Text)
	}
	if len(deltas) != 1 {
		t.Errorf("expected 1 delta, got %d", len(deltas))
	}
}

func TestParseStreamEvents_NilCallback(t *testing.T) {
	input := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"data"}}}
{"type":"result","result":"data","session_id":"s1"}`

	// Should not panic with nil onUpdate
	result := parseStreamEvents(strings.NewReader(input), nil)
	if result.Text != "data" {
		t.Errorf("expected 'data', got %q", result.Text)
	}
}

func TestExtractContentText(t *testing.T) {
	content := []streamContent{
		{Type: "text", Text: "Hello "},
		{Type: "tool_use", Text: "ignored"},
		{Type: "text", Text: "world"},
	}
	got := extractContentText(content)
	if got != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", got)
	}
}

func TestExtractContentText_Empty(t *testing.T) {
	got := extractContentText(nil)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
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
