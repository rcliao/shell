package daemon

import "testing"

func TestParseUserLabels(t *testing.T) {
	out := parseUserLabels(map[string]string{
		"123":      "Alex (the developer)",
		"not-a-id": "ignored",
		"456":      "Sam",
	})
	if len(out) != 2 {
		t.Fatalf("want 2 entries, got %d: %v", len(out), out)
	}
	if out[123] != "Alex (the developer)" || out[456] != "Sam" {
		t.Errorf("unexpected map: %v", out)
	}
	if parseUserLabels(nil) != nil {
		t.Error("nil input should return nil")
	}
}
