package project

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// --- fakes ---

type fakeHomeStore struct {
	projects []store.Project
	pins     map[int64]int
}

func (f *fakeHomeStore) ListProjects(chatID int64) ([]store.Project, error) {
	return f.projects, nil
}

func (f *fakeHomeStore) GetChatPin(chatID int64) (*store.ChatPin, error) {
	if id, ok := f.pins[chatID]; ok {
		return &store.ChatPin{ChatID: chatID, ProjectsMsgID: id}, nil
	}
	return nil, nil
}

func (f *fakeHomeStore) SetChatPin(chatID int64, msgID int) error {
	if f.pins == nil {
		f.pins = map[int64]int{}
	}
	f.pins[chatID] = msgID
	return nil
}

type fakeHomeTransport struct {
	sent    []string // texts sent as fresh messages
	edits   []int    // message ids edited
	pins    []int    // message ids pinned
	unpins  []int    // message ids unpinned
	nextID  int
	editErr error
}

func (f *fakeHomeTransport) SendMessageID(chatID, threadID int64, text string) (int, error) {
	f.nextID++
	f.sent = append(f.sent, text)
	return f.nextID, nil
}

func (f *fakeHomeTransport) EditMessage(chatID int64, messageID int, text string) error {
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, messageID)
	return nil
}

func (f *fakeHomeTransport) PinMessage(chatID int64, messageID int, silent bool) error {
	f.pins = append(f.pins, messageID)
	return nil
}

func (f *fakeHomeTransport) UnpinMessage(chatID int64, messageID int) error {
	f.unpins = append(f.unpins, messageID)
	return nil
}

// --- RenderHome ---

func TestRenderHomeEmpty(t *testing.T) {
	got := RenderHome(nil)
	if !strings.HasPrefix(got, "📋 Projects") || !strings.Contains(got, "(no active projects)") {
		t.Errorf("empty render = %q", got)
	}
}

func TestRenderHomeRows(t *testing.T) {
	at := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	later := at.Add(24 * time.Hour)
	projects := []store.Project{
		{Title: "Housing Search", Emoji: "🏠", Status: "active", DocPath: "projects/housing/doc.md",
			UpdatedAt: at, LastResearchAt: &later},
		{Title: "Old Thing", Status: "archived", UpdatedAt: at},
		{Title: "Napping", Status: "paused", UpdatedAt: at},
		{Title: "No Doc Yet", Status: "active", UpdatedAt: at},
	}
	got := RenderHome(projects)

	if !strings.Contains(got, "🏠 Housing Search — 2026-08-18 📄") {
		t.Errorf("active row missing/wrong (want latest activity date and doc marker):\n%s", got)
	}
	if !strings.Contains(got, "No Doc Yet — 2026-08-17") {
		t.Errorf("docless active row missing:\n%s", got)
	}
	if strings.Contains(got, "No Doc Yet — 2026-08-17 📄") {
		t.Errorf("docless row should not carry the doc marker:\n%s", got)
	}
	// Archived and paused both leave the pinned list (plan §Lifecycle).
	if strings.Contains(got, "Old Thing") || strings.Contains(got, "Napping") {
		t.Errorf("non-active projects leaked into the home:\n%s", got)
	}
}

// --- Refresh / Repin ---

func activeProject(title string) store.Project {
	return store.Project{Title: title, Status: "active", UpdatedAt: time.Now().UTC()}
}

func TestRefreshFirstTimeSendsPinsAndRecords(t *testing.T) {
	st := &fakeHomeStore{projects: []store.Project{activeProject("Demo")}}
	tr := &fakeHomeTransport{}
	h := NewHome(st, tr)

	h.Refresh(42)

	if len(tr.sent) != 1 || len(tr.pins) != 1 {
		t.Fatalf("sent=%d pins=%d, want one send + one pin", len(tr.sent), len(tr.pins))
	}
	if st.pins[42] != tr.pins[0] {
		t.Errorf("recorded pin %d != pinned msg %d", st.pins[42], tr.pins[0])
	}
}

func TestRefreshEditsInPlaceAndDebounces(t *testing.T) {
	st := &fakeHomeStore{projects: []store.Project{activeProject("Demo")}, pins: map[int64]int{42: 9}}
	tr := &fakeHomeTransport{}
	h := NewHome(st, tr)

	h.Refresh(42)
	if len(tr.edits) != 1 || tr.edits[0] != 9 {
		t.Fatalf("edits = %v, want one edit of msg 9", tr.edits)
	}
	if len(tr.sent) != 0 {
		t.Errorf("no fresh send expected when a pin exists")
	}

	// Second refresh inside the debounce window is skipped.
	h.Refresh(42)
	if len(tr.edits) != 1 {
		t.Errorf("edits = %v, want the second refresh debounced", tr.edits)
	}

	// A different chat is not debounced by chat 42's timestamp.
	st.pins[7] = 3
	h.Refresh(7)
	if len(tr.edits) != 2 {
		t.Errorf("edits = %v, want chat 7 edited independently", tr.edits)
	}
}

func TestRefreshResendsWhenPinnedMessageGone(t *testing.T) {
	st := &fakeHomeStore{projects: []store.Project{activeProject("Demo")}, pins: map[int64]int{42: 9}}
	tr := &fakeHomeTransport{editErr: fmt.Errorf("Bad Request: message to edit not found")}
	h := NewHome(st, tr)

	h.Refresh(42)

	if len(tr.sent) != 1 || len(tr.pins) != 1 {
		t.Fatalf("sent=%d pins=%d, want a fresh send + pin after the edit target vanished", len(tr.sent), len(tr.pins))
	}
	if st.pins[42] == 9 {
		t.Error("pin row should point at the fresh message, not the deleted one")
	}
}

func TestRefreshOtherEditErrorDoesNotResend(t *testing.T) {
	st := &fakeHomeStore{projects: []store.Project{activeProject("Demo")}, pins: map[int64]int{42: 9}}
	tr := &fakeHomeTransport{editErr: fmt.Errorf("network is down")}
	h := NewHome(st, tr)

	h.Refresh(42)

	if len(tr.sent) != 0 {
		t.Errorf("a transient edit failure must not spawn a duplicate home message")
	}
}

func TestRepinSendsFreshUnpinsOldBypassesDebounce(t *testing.T) {
	st := &fakeHomeStore{projects: []store.Project{activeProject("Demo")}, pins: map[int64]int{42: 9}}
	tr := &fakeHomeTransport{}
	h := NewHome(st, tr)

	// Burn the debounce window with an edit; /projects must still act.
	h.Refresh(42)
	if err := h.Repin(42, 1650); err != nil {
		t.Fatal(err)
	}

	if len(tr.sent) != 1 || len(tr.pins) != 1 {
		t.Fatalf("sent=%d pins=%d, want a fresh send + pin", len(tr.sent), len(tr.pins))
	}
	if len(tr.unpins) != 1 || tr.unpins[0] != 9 {
		t.Errorf("unpins = %v, want the old message 9 unpinned", tr.unpins)
	}
	if st.pins[42] != tr.pins[0] {
		t.Errorf("recorded pin %d != pinned msg %d", st.pins[42], tr.pins[0])
	}
}

func TestRenderHomeRecentCommentMarker(t *testing.T) {
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	stale := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	projects := []store.Project{
		{Title: "Chatty", Status: "active",
			HandledDiscussions: `[{"id":"d1","at":"` + recent + `"}]`},
		{Title: "Quiet", Status: "active",
			HandledDiscussions: `[{"id":"d2","at":"` + stale + `"}, "legacy-id"]`},
	}
	text := RenderHome(projects)
	lines := strings.Split(text, "\n")
	if len(lines) != 3 {
		t.Fatalf("home = %q", text)
	}
	if !strings.Contains(lines[1], "💬") {
		t.Errorf("recent-comment project missing 💬: %q", lines[1])
	}
	if strings.Contains(lines[2], "💬") {
		t.Errorf("stale/legacy discussions must not mark 💬: %q", lines[2])
	}
}
