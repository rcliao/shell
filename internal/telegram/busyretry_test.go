package telegram

import (
	"errors"
	"testing"

	"github.com/rcliao/shell/internal/process"
)

// A user message must outlast a synthetic turn holding the session. On 7/20
// an A2A hand-off from the peer agent took the Claude session and the owner's
// group message was dropped outright — the user path had no retry while
// synthetic senders did. Guard the pacing that gives the user the last word.
func TestUserBusyRetryOutlastsSyntheticTurn(t *testing.T) {
	if len(userBusyRetryDelays) < 3 {
		t.Fatalf("want at least 3 user retries, got %d", len(userBusyRetryDelays))
	}
	var total float64
	prev := -1.0
	for i, d := range userBusyRetryDelays {
		secs := d.Seconds()
		if secs <= prev {
			t.Errorf("delay %d (%v) must back off beyond previous %.0fs", i, d, prev)
		}
		prev = secs
		total += secs
	}
	// Synthetic turns retry on a 30s cadence; the user budget must cover a
	// typical long turn rather than giving up while one is still running.
	if total < 30 {
		t.Errorf("total user retry budget %.0fs is too short to outlast a long synthetic turn", total)
	}
	if !errors.Is(process.ErrSessionBusy, process.ErrSessionBusy) {
		t.Fatal("sanity: ErrSessionBusy must be comparable with errors.Is")
	}
}
