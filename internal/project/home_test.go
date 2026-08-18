package project

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/bridge"
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
	sent        []string              // texts sent as fresh messages
	sentButtons [][]bridge.LinkButton // buttons per fresh send
	edits       []int                 // message ids edited
	editButtons [][]bridge.LinkButton // buttons per edit
	pins        []int                 // message ids pinned
	unpins      []int                 // message ids unpinned
	nextID      int
	editErr     error
}

func (f *fakeHomeTransport) SendMessageIDButtons(chatID, threadID int64, text string, buttons []bridge.LinkButton) (int, error) {
	f.nextID++
	f.sent = append(f.sent, text)
	f.sentButtons = append(f.sentButtons, buttons)
	return f.nextID, nil
}

func (f *fakeHomeTransport) EditMessageButtons(chatID int64, messageID int, text string, buttons []bridge.LinkButton) error {
	if f.editErr != nil {
		return f.editErr
	}
	f.edits = append(f.edits, messageID)
	f.editButtons = append(f.editButtons, buttons)
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

// --- HomeButtons ---

// renderedBlockMap is the minimal block map that counts as "WE rendered this
// page" — the NotionPageURL guard.
const renderedBlockMap = `{"sections":{"## Overview":{"hash":"h1","blocks":["b1"]}}}`

func exportedProject(title, emoji, ref string) store.Project {
	return store.Project{
		Title: title, Emoji: emoji, Status: "active", UpdatedAt: time.Now().UTC(),
		ExportKind: "notion", ExportRef: ref, BlockMap: renderedBlockMap,
	}
}

func TestHomeButtonsOrderLabelsAndGuards(t *testing.T) {
	projects := []store.Project{
		exportedProject("Housing Search", "🏠", "aaaa1111-2222-4333-8444-555566667777"),
		// Archived: no line, no button.
		exportedProject("Done Thing", "", "bbbb1111-2222-4333-8444-555566667777"),
		// No export: line yes, button no.
		activeProject("Doc-less"),
		// export_ref without a rendered block map (human page / database id):
		// no button — the URL shape is not ours to guess.
		{Title: "Human Page", Status: "active", UpdatedAt: time.Now().UTC(),
			ExportKind: "notion", ExportRef: "cccc1111-2222-4333-8444-555566667777"},
		exportedProject("Trip Planning", "", "dddd1111-2222-4333-8444-555566667777"),
	}
	projects[1].Status = "archived"

	got := HomeButtons(projects)
	if len(got) != 2 {
		t.Fatalf("buttons = %+v, want exactly the two exported active projects", got)
	}
	if got[0].Label != "📄 🏠 Housing Search" || got[0].URL != "https://notion.so/aaaa1111222243338444555566667777" {
		t.Errorf("first button = %+v", got[0])
	}
	if got[1].Label != "📄 Trip Planning" || !strings.HasPrefix(got[1].URL, "https://notion.so/dddd1111") {
		t.Errorf("second button = %+v (emoji-less label must not double-space)", got[1])
	}
}

func TestHomeButtonsCapAndTruncation(t *testing.T) {
	var projects []store.Project
	for i := 0; i < maxHomeButtons+2; i++ {
		ref := fmt.Sprintf("%04d1111-2222-4333-8444-555566667777", i)
		projects = append(projects, exportedProject(fmt.Sprintf("Project %d", i), "", ref))
	}
	projects[0].Title = strings.Repeat("長", maxButtonTitleRunes+10)

	got := HomeButtons(projects)
	if len(got) != maxHomeButtons {
		t.Fatalf("buttons = %d, want capped at %d", len(got), maxHomeButtons)
	}
	wantTitle := strings.Repeat("長", maxButtonTitleRunes-1) + "…"
	if got[0].Label != "📄 "+wantTitle {
		t.Errorf("truncated label = %q, want %d-rune title ending in …", got[0].Label, maxButtonTitleRunes)
	}
	// Order matches list order; the overflow projects are the ones dropped.
	if got[maxHomeButtons-1].Label != fmt.Sprintf("📄 Project %d", maxHomeButtons-1) {
		t.Errorf("last button = %+v, want list order preserved", got[maxHomeButtons-1])
	}
}

func TestRefreshCarriesButtons(t *testing.T) {
	st := &fakeHomeStore{projects: []store.Project{
		exportedProject("Linked", "🔗", "eeee1111-2222-4333-8444-555566667777"),
		activeProject("Plain"),
	}}
	tr := &fakeHomeTransport{}
	h := NewHome(st, tr)

	// Fresh send carries the buttons.
	h.Refresh(42)
	if len(tr.sentButtons) != 1 || len(tr.sentButtons[0]) != 1 {
		t.Fatalf("sent buttons = %+v, want one button on the fresh home", tr.sentButtons)
	}
	if tr.sentButtons[0][0].Label != "📄 🔗 Linked" {
		t.Errorf("button = %+v", tr.sentButtons[0][0])
	}

	// Repin (bypasses the debounce) edits nothing but re-sends with buttons.
	if err := h.Repin(42, 0); err != nil {
		t.Fatal(err)
	}
	if len(tr.sentButtons) != 2 || len(tr.sentButtons[1]) != 1 {
		t.Fatalf("repin buttons = %+v", tr.sentButtons)
	}
}

func TestRefreshEditCarriesButtons(t *testing.T) {
	st := &fakeHomeStore{
		projects: []store.Project{exportedProject("Linked", "", "ffff1111-2222-4333-8444-555566667777")},
		pins:     map[int64]int{42: 9},
	}
	tr := &fakeHomeTransport{}
	h := NewHome(st, tr)

	h.Refresh(42)
	if len(tr.editButtons) != 1 || len(tr.editButtons[0]) != 1 {
		t.Fatalf("edit buttons = %+v, want the button on the in-place edit", tr.editButtons)
	}
	if tr.editButtons[0][0].URL != "https://notion.so/ffff1111222243338444555566667777" {
		t.Errorf("edit button url = %q", tr.editButtons[0][0].URL)
	}
}
