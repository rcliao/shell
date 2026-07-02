package topic

import (
	"strings"
	"testing"
)

func TestParseHaikuJSON(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantTopic string
		wantErr bool
	}{
		{
			name:      "plain json",
			input:     `{"topic":"plants","is_new":false,"confidence":0.92}`,
			wantTopic: "plants",
		},
		{
			name:      "json with code fence",
			input:     "```json\n{\"topic\":\"meals\",\"is_new\":false,\"confidence\":0.85}\n```",
			wantTopic: "meals",
		},
		{
			name:      "json with leading prose",
			input:     "Based on the message, this is about plants.\n\n{\"topic\":\"plants\",\"is_new\":false,\"confidence\":0.9}",
			wantTopic: "plants",
		},
		{
			name:      "new topic with description",
			input:     `{"topic":"image_gen","is_new":true,"description":"image generation requests","confidence":0.88}`,
			wantTopic: "image_gen",
		},
		{
			name:    "no json",
			input:   "Sorry, I cannot classify this.",
			wantErr: true,
		},
		{
			name:    "empty topic",
			input:   `{"topic":"","is_new":false,"confidence":0.1}`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := parseHaikuJSON(c.input)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got result %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.Topic != c.wantTopic {
				t.Errorf("topic = %q, want %q", r.Topic, c.wantTopic)
			}
		})
	}
}

func TestBuildPromptFormatting(t *testing.T) {
	topics := []Topic{
		{Name: "plants", Description: "houseplant care, soil moisture"},
		{Name: "meals", Description: "breakfast/lunch/dinner memos"},
	}
	out := buildPrompt("my brazilian wood needs water", topics)
	if !strings.Contains(out, "plants: houseplant care, soil moisture") {
		t.Errorf("prompt missing plants entry; got:\n%s", out)
	}
	if !strings.Contains(out, "my brazilian wood needs water") {
		t.Errorf("prompt missing user message")
	}

	// Empty registry
	out = buildPrompt("hi", nil)
	if !strings.Contains(out, "(none yet") {
		t.Errorf("empty-registry prompt missing fallback line; got:\n%s", out)
	}
}
