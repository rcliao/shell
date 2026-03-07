package imagen

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestNewRequiresAPIKey(t *testing.T) {
	_, err := New("", "", 0)
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
}

func TestNewDefaults(t *testing.T) {
	// We can't actually create a client without a valid key,
	// but we can verify the constructor doesn't panic with a dummy key.
	// The genai client creation may fail with an invalid key at request time.
	g, err := New("test-key", "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if g.model != "gemini-3.1-flash-image-preview" {
		t.Errorf("expected default model, got %q", g.model)
	}
	if g.timeout != 2*time.Minute {
		t.Errorf("expected 2m timeout, got %v", g.timeout)
	}
}

func TestGenerateIntegration(t *testing.T) {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set, skipping integration test")
	}

	g, err := New(apiKey, "", 0)
	if err != nil {
		t.Fatalf("failed to create generator: %v", err)
	}

	data, err := g.Generate(context.Background(), "a simple red circle on a white background")
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty image data")
	}
}
