// Package decide asks typed questions of a decision model and returns typed
// answers: a choice with probabilities, or a 0–1 belief. It exists so the
// bridge can make small routing decisions — which project is this about,
// should this agent reply at all — without spending a chat-model turn, and
// so the model behind those decisions is swappable.
//
// The first backend is TypeSafe's Jev (plan P3.7). It runs in SHADOW: every
// answer is logged beside what actually happened and nothing acts on it,
// until the logged rows say it may.
package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// A Question is one typed ask. Choice questions carry Criteria (option key →
// description); Noul questions ask for a belief that Instructions is true.
type Question struct {
	Type         string            `json:"type"` // "choice" | "noul"
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// An Answer is the typed reply to one Question. For a choice: Choice,
// Probabilities and Confidence. For a noul: Noul.
type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
}

// Usage is what one request cost in tokens; pricing is not published yet,
// so tokens are what gets recorded.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Result is one round trip: answers keyed like the questions, plus cost.
type Result struct {
	Model   string
	Answers map[string]Answer
	Usage   Usage
	Latency time.Duration
}

// Decider answers a batch of questions about one state in one call. Every
// question is evaluated independently against the same state.
type Decider interface {
	// Enabled reports whether Ask can succeed at all (a key is present).
	Enabled() bool
	Ask(ctx context.Context, state any, questions map[string]Question) (Result, error)
}

const (
	jevURL       = "https://api.typesafe.ai/v1/systemone"
	jevModel     = "jev-latest"
	jevKeyEnv    = "TYPESAFE_API_KEY"
	jevMaxChoice = 255
)

// Jev is the TypeSafe System One client: one POST, bearer auth.
type Jev struct {
	httpc *http.Client
	url   string
	key   func() string
}

// NewJev reads the key from the environment at call time, so a key added
// to .env after start is picked up on the next restart without a rebuild.
func NewJev() *Jev {
	return &Jev{
		httpc: &http.Client{Timeout: 10 * time.Second},
		url:   jevURL,
		key:   func() string { return os.Getenv(jevKeyEnv) },
	}
}

func (j *Jev) Enabled() bool { return j.key() != "" }

// APIError is a non-2xx reply. Retryable marks 429/529, which the caller
// may back off on; in shadow mode it just records the miss.
type APIError struct {
	Status    int
	Body      string
	Retryable bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jev: HTTP %d: %s", e.Status, truncate(e.Body, 200))
}

// Ask sends state + questions and decodes the typed answers.
func (j *Jev) Ask(ctx context.Context, state any, questions map[string]Question) (Result, error) {
	if !j.Enabled() {
		return Result{}, errors.New("jev: " + jevKeyEnv + " not set")
	}
	if len(questions) == 0 {
		return Result{}, errors.New("jev: no questions")
	}
	for id, q := range questions {
		if q.Type == "choice" && len(q.Criteria) > jevMaxChoice {
			return Result{}, fmt.Errorf("jev: question %q has %d options, max %d", id, len(q.Criteria), jevMaxChoice)
		}
	}
	body, err := json.Marshal(map[string]any{"state": state, "model": jevModel, "questions": questions})
	if err != nil {
		return Result{}, fmt.Errorf("jev: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+j.key())
	req.Header.Set("Content-Type", "application/json")

	started := time.Now()
	resp, err := j.httpc.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("jev: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Result{}, fmt.Errorf("jev: read: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return Result{}, &APIError{Status: resp.StatusCode, Body: string(raw),
			Retryable: resp.StatusCode == 429 || resp.StatusCode == 529}
	}
	var out struct {
		Model   string            `json:"model"`
		Answers map[string]Answer `json:"answers"`
		Usage   Usage             `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("jev: decode: %w", err)
	}
	return Result{Model: out.Model, Answers: out.Answers, Usage: out.Usage, Latency: time.Since(started)}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
