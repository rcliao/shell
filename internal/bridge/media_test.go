package bridge

import (
	"testing"
	"time"
)

func TestExtractMediaNote(t *testing.T) {
	tests := []struct {
		name, in, wantClean, wantNote string
	}{
		{"note at end",
			"看到了，這是太陽能板背面\n\n[media-note: solar panel mounted on fascia board, angled bracket]",
			"看到了，這是太陽能板背面",
			"solar panel mounted on fascia board, angled bracket"},
		{"note with colon omitted",
			"answer text [media-note 花梗特寫，兩個節點]",
			"answer text",
			"花梗特寫，兩個節點"},
		{"no note", "just an answer", "just an answer", ""},
		{"note mid-text stripped",
			"before [media-note: cat photo] after",
			"before  after",
			"cat photo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, note := extractMediaNote(tt.in)
			if clean != tt.wantClean || note != tt.wantNote {
				t.Fatalf("got (%q, %q), want (%q, %q)", clean, note, tt.wantClean, tt.wantNote)
			}
		})
	}
}

func TestHumanAge(t *testing.T) {
	for _, tt := range []struct {
		d    time.Duration
		want string
	}{
		{35 * time.Minute, "35m ago"},
		{6 * time.Hour, "6h ago"},
		{72 * time.Hour, "3d ago"},
	} {
		if got := humanAge(tt.d); got != tt.want {
			t.Fatalf("humanAge(%v)=%q want %q", tt.d, got, tt.want)
		}
	}
}
