package imagen

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/genai"
)

type Generator struct {
	client  *genai.Client
	model   string
	timeout time.Duration
}

func New(apiKey, model string, timeout time.Duration) (*Generator, error) {
	if apiKey == "" {
		return nil, fmt.Errorf("Google API key is empty")
	}
	client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, fmt.Errorf("create genai client: %w", err)
	}
	if model == "" {
		model = "gemini-3.1-flash-image-preview"
	}
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	return &Generator{
		client:  client,
		model:   model,
		timeout: timeout,
	}, nil
}

func (g *Generator) Generate(ctx context.Context, prompt string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	resp, err := g.client.Models.GenerateContent(ctx, g.model, []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}, &genai.GenerateContentConfig{
		ResponseModalities: []string{"IMAGE", "TEXT"},
	})
	if err != nil {
		return nil, fmt.Errorf("generate content: %w", err)
	}

	for _, cand := range resp.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && len(part.InlineData.Data) > 0 {
				return part.InlineData.Data, nil
			}
		}
	}

	return nil, fmt.Errorf("no image data in response")
}
