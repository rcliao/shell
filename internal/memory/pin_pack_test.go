package memory

import (
	"strings"
	"testing"

	agentmemory "github.com/rcliao/ghost"
)

// The system-prompt cut reported to the deep beat must be the cut the
// composed prompt applies: same order, same byte accounting, same skip
// behaviour (a pin that no longer fits is skipped, later smaller ones may
// still be admitted).
func TestPackOperatingPinsMatchesPromptPacking(t *testing.T) {
	mk := func(key string, n int, tags ...string) agentmemory.Memory {
		return agentmemory.Memory{Key: key, Content: strings.Repeat("x", n), Tags: tags}
	}
	pinned := []agentmemory.Memory{
		mk("newest", 300),
		mk("charter", 5000, "layer:charter"), // identity layer: never packed, never cut
		mk("big-old", 500),
		mk("small-old", 100),
		mk("oldest", 100),
	}
	operating, layerOf := splitOperatingPins(pinned)
	if _, ok := layerOf["charter"]; !ok || len(operating) != 4 {
		t.Fatalf("split: layerOf=%v operating=%d", layerOf, len(operating))
	}
	// budget 150 tokens = 600 bytes: newest(300) + big-old(500) overflows →
	// big-old skipped; small-old(100) + oldest(100) still fit.
	kept, dropped := packOperatingPins(operating, 150)
	keys := func(ms []agentmemory.Memory) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Key)
		}
		return out
	}
	if got := strings.Join(keys(kept), ","); got != "newest,small-old,oldest" {
		t.Errorf("kept = %s", got)
	}
	if got := strings.Join(keys(dropped), ","); got != "big-old" {
		t.Errorf("dropped = %s", got)
	}
}
