package judge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func cliOut(result string) []byte {
	return []byte(`{"type":"result","is_error":false,"result":` + quote(result) + `,"total_cost_usd":0.012,"usage":{"input_tokens":900,"output_tokens":40}}`)
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParse(t *testing.T) {
	v, err := Parse(cliOut(`{"verdict": "met", "reason": "kit B at $38 is linked"}`))
	if err != nil || v.Verdict != Met || v.Reason != "kit B at $38 is linked" || v.CostUSD != 0.012 || v.InputTokens != 900 {
		t.Fatalf("met: %+v %v", v, err)
	}
	// The CLI's array form: events ending in the result.
	arr := []byte(`[{"type":"system"},{"type":"assistant"},` + string(cliOut(`{"verdict": "met", "reason": "ok"}`)) + `]`)
	if v, err := Parse(arr); err != nil || v.Verdict != Met || v.CostUSD != 0.012 {
		t.Errorf("array: %+v %v", v, err)
	}
	if _, err := Parse([]byte(`[{"type":"system"}]`)); err == nil {
		t.Error("array without a result parsed")
	}
	// Prose around the JSON is tolerated.
	if v, err := Parse(cliOut("Here you go:\n```json\n{\"verdict\": \"not_met\", \"reason\": \"no link\"}\n```")); err != nil || v.Verdict != NotMet {
		t.Errorf("fenced: %+v %v", v, err)
	}
	for name, out := range map[string][]byte{
		"not json":        []byte("Sure! It is met."),
		"no verdict json": cliOut("It is met."),
		"unknown verdict": cliOut(`{"verdict": "probably", "reason": "x"}`),
		"cli error":       []byte(`{"is_error":true,"result":"overloaded","total_cost_usd":0}`),
	} {
		if _, err := Parse(out); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestDoneDefaultsToNotMet(t *testing.T) {
	calls := 0
	runner := func(out []byte, err error) Runner {
		return func(_ context.Context, prompt string) ([]byte, error) {
			calls++
			if !strings.Contains(prompt, "Done criteria: kit in doc") || !strings.Contains(prompt, "B $38") {
				t.Errorf("prompt lacks the claim:\n%s", prompt)
			}
			return out, err
		}
	}
	ctx := context.Background()

	if v, err := Done(ctx, runner(nil, nil), 0, "g", "kit in doc", "  "); err != nil || v.Verdict != NotMet || calls != 0 {
		t.Errorf("empty evidence: %+v %v, %d calls", v, err, calls)
	}
	if v, err := Done(ctx, runner(nil, errors.New("timeout")), 0, "g", "kit in doc", "B $38"); err == nil || v.Verdict != NotMet || !strings.HasPrefix(v.Reason, "judge failed") {
		t.Errorf("runner error: %+v %v", v, err)
	}
	if v, err := Done(ctx, runner([]byte("garbage"), nil), 0, "g", "kit in doc", "B $38"); err == nil || v.Verdict != NotMet {
		t.Errorf("unparseable: %+v %v", v, err)
	}
	if v, err := Done(ctx, runner(cliOut(`{"verdict":"met","reason":"ok"}`), nil), 0, "g", "kit in doc", "B $38"); err != nil || v.Verdict != Met {
		t.Errorf("met: %+v %v", v, err)
	}
}
