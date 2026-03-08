package browser

import (
	"testing"
	"time"
)

func TestParseDirective_Basic(t *testing.T) {
	body := `
navigate
click "#login-button"
type "#email" "user@example.com"
type "#password" "secret123"
click "#submit"
wait "#dashboard"
screenshot
extract "#welcome-message"
js "document.title"
sleep "2s"
`
	d := ParseDirective("https://example.com", body)

	if d.URL != "https://example.com" {
		t.Errorf("URL = %q, want %q", d.URL, "https://example.com")
	}

	if len(d.Actions) != 10 {
		t.Fatalf("got %d actions, want 10", len(d.Actions))
	}

	tests := []struct {
		typ      ActionType
		selector string
		value    string
	}{
		{ActionNavigate, "", "https://example.com"},
		{ActionClick, "#login-button", ""},
		{ActionType_, "#email", "user@example.com"},
		{ActionType_, "#password", "secret123"},
		{ActionClick, "#submit", ""},
		{ActionWait, "#dashboard", ""},
		{ActionScreenshot, "", ""},
		{ActionExtract, "#welcome-message", ""},
		{ActionJS, "", "document.title"},
		{ActionSleep, "", "2s"},
	}

	for i, tt := range tests {
		a := d.Actions[i]
		if a.Type != tt.typ {
			t.Errorf("action[%d] type = %d, want %d", i, a.Type, tt.typ)
		}
		if a.Selector != tt.selector {
			t.Errorf("action[%d] selector = %q, want %q", i, a.Selector, tt.selector)
		}
		if a.Value != tt.value {
			t.Errorf("action[%d] value = %q, want %q", i, a.Value, tt.value)
		}
	}
}

func TestParseDirective_EmptyBody(t *testing.T) {
	d := ParseDirective("https://example.com", "")
	if len(d.Actions) != 0 {
		t.Errorf("got %d actions for empty body, want 0", len(d.Actions))
	}
}

func TestParseDirective_ScreenshotOnly(t *testing.T) {
	d := ParseDirective("https://example.com", "screenshot")
	if len(d.Actions) != 1 {
		t.Fatalf("got %d actions, want 1", len(d.Actions))
	}
	if d.Actions[0].Type != ActionScreenshot {
		t.Errorf("action type = %d, want ActionScreenshot", d.Actions[0].Type)
	}
}

func TestParseDirective_SkipsBlankLines(t *testing.T) {
	body := `
click "#btn1"

click "#btn2"
`
	d := ParseDirective("https://example.com", body)
	if len(d.Actions) != 2 {
		t.Fatalf("got %d actions, want 2", len(d.Actions))
	}
}

func TestParseSleepDuration(t *testing.T) {
	tests := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"2s", 2 * time.Second, false},
		{"500ms", 500 * time.Millisecond, false},
		{"1m", 30 * time.Second, false}, // capped at 30s
		{"bad", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseSleepDuration(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseSleepDuration(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSleepDuration(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestBrowserRe(t *testing.T) {
	input := `Some text before [browser url="https://example.com"]
click "#btn"
screenshot
[/browser] some text after`

	matches := BrowserRe.FindAllStringSubmatch(input, -1)
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0][1] != "https://example.com" {
		t.Errorf("URL = %q, want %q", matches[0][1], "https://example.com")
	}
	body := matches[0][2]
	if !contains(body, "click") || !contains(body, "screenshot") {
		t.Errorf("body = %q, expected click and screenshot", body)
	}
}

func TestActionString(t *testing.T) {
	a := Action{Type: ActionClick, Selector: "#btn"}
	s := a.String()
	if s != `click "#btn"` {
		t.Errorf("String() = %q, want %q", s, `click "#btn"`)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
