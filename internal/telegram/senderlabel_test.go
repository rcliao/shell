package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

// senderLabel resolves the [From: ...] display name: configured label wins,
// then first name, then username. Group chats depend on the label carrying
// real identity — a bare first name is not enough for the agent to know who
// is speaking (observed misattribution in the multi-user group, 2026-07-23).
func TestSenderLabel(t *testing.T) {
	h := &Handler{userLabels: map[int64]string{
		42: "Alex (the developer)",
		99: "", // empty label must fall through, not blank the name
	}}

	cases := []struct {
		name string
		from *models.User
		want string
	}{
		{"configured label wins", &models.User{ID: 42, FirstName: "Alex", Username: "alex_dev"}, "Alex (the developer)"},
		{"no label falls back to first name", &models.User{ID: 7, FirstName: "Sam", Username: "sam_u"}, "Sam"},
		{"empty first name falls back to username", &models.User{ID: 7, Username: "sam_u"}, "sam_u"},
		{"empty configured label falls through", &models.User{ID: 99, FirstName: "Kim"}, "Kim"},
		{"nil user", nil, ""},
	}
	for _, tc := range cases {
		if got := h.senderLabel(tc.from); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A handler with no userLabels map at all must not panic.
func TestSenderLabelNilMap(t *testing.T) {
	h := &Handler{}
	if got := h.senderLabel(&models.User{ID: 1, FirstName: "Sam"}); got != "Sam" {
		t.Errorf("got %q, want Sam", got)
	}
}
