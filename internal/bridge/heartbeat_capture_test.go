package bridge

import (
	"strings"
	"testing"
)

// The journal must capture DEEP beats and nothing else. A shallow beat's
// output ceiling is a status line that gets discarded downstream; journaling
// it would bury the ~1-in-6 turns that actually contain self-audit.
func TestDeepHeartbeatPrefixSelectsOnlyDeepBeats(t *testing.T) {
	cases := []struct {
		msg  string
		deep bool
	}{
		{"[Heartbeat:deep] Review recent activity", true},
		{"[Heartbeat] Review recent activity", false},
		{"what's for lunch?", false},
		{"", false},
		// Near-misses: the prefix is matched exactly, including its trailing
		// space, so a message merely mentioning deep heartbeats is not journaled.
		{"[Heartbeat:deep]no-space", false},
		{"talking about [Heartbeat:deep] beats", false},
	}
	for _, c := range cases {
		got := strings.HasPrefix(c.msg, deepHeartbeatPrefix)
		if got != c.deep {
			t.Errorf("HasPrefix(%q) = %v, want %v", c.msg, got, c.deep)
		}
	}
}

// The prefix constant must stay identical to what the scheduler prepends —
// they live in different packages, and a silent drift would stop capture dead
// while everything still looked healthy.
func TestDeepHeartbeatPrefixMatchesSchedulerMarker(t *testing.T) {
	const schedulerMarker = "[Heartbeat:deep] " // scheduler.execute() prepends this
	if deepHeartbeatPrefix != schedulerMarker {
		t.Fatalf("prefix drift: bridge has %q, scheduler emits %q — capture would silently stop",
			deepHeartbeatPrefix, schedulerMarker)
	}
}
