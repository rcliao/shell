package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTransport records every call, and is the compile-time proof that a
// Transport implementation must cover the P2 additions (document, pin/unpin,
// send-returning-id, edit).
type fakeTransport struct {
	notified  []string
	documents []DocumentAttachment
	sentIDs   []string
	edits     map[int]string
	pinned    map[int]bool
	nextID    int
	// buttons records the LinkButtons attached to each button-variant call,
	// in call order (nil entries for buttonless sends).
	buttons [][]LinkButton
}

var _ Transport = (*fakeTransport)(nil)

func newFakeTransport() *fakeTransport {
	return &fakeTransport{edits: map[int]string{}, pinned: map[int]bool{}, nextID: 100}
}

func (f *fakeTransport) Notify(chatID, threadID int64, msg string) {
	f.notified = append(f.notified, msg)
}
func (f *fakeTransport) SendPhoto(chatID, threadID int64, data []byte, caption string) {}
func (f *fakeTransport) SendVideo(chatID, threadID int64, data []byte, caption string) {}

func (f *fakeTransport) SendDocument(chatID, threadID int64, path, caption string) error {
	f.documents = append(f.documents, DocumentAttachment{Path: path, Caption: caption})
	return nil
}

func (f *fakeTransport) SendMessageID(chatID, threadID int64, text string) (int, error) {
	f.nextID++
	f.sentIDs = append(f.sentIDs, text)
	return f.nextID, nil
}

func (f *fakeTransport) EditMessage(chatID int64, messageID int, text string) error {
	f.edits[messageID] = text
	return nil
}

func (f *fakeTransport) PinMessage(chatID int64, messageID int, silent bool) error {
	f.pinned[messageID] = true
	return nil
}

func (f *fakeTransport) UnpinMessage(chatID int64, messageID int) error {
	delete(f.pinned, messageID)
	return nil
}

func (f *fakeTransport) NotifyButtons(chatID, threadID int64, text string, buttons []LinkButton) error {
	f.notified = append(f.notified, text)
	f.buttons = append(f.buttons, buttons)
	return nil
}

func (f *fakeTransport) SendMessageIDButtons(chatID, threadID int64, text string, buttons []LinkButton) (int, error) {
	f.buttons = append(f.buttons, buttons)
	return f.SendMessageID(chatID, threadID, text)
}

func (f *fakeTransport) EditMessageButtons(chatID int64, messageID int, text string, buttons []LinkButton) error {
	f.buttons = append(f.buttons, buttons)
	return f.EditMessage(chatID, messageID, text)
}

// The pinned-message lifecycle the project home needs: send returns an id the
// caller can pin, edit in place, and unpin — all through one Transport.
func TestTransportPinnedMessageLifecycle(t *testing.T) {
	ft := newFakeTransport()

	id, err := ft.SendMessageID(42, 0, "list v1")
	if err != nil || id == 0 {
		t.Fatalf("SendMessageID = (%d, %v), want a real id", id, err)
	}
	if err := ft.PinMessage(42, id, true); err != nil || !ft.pinned[id] {
		t.Fatalf("pin failed: %v", err)
	}
	if err := ft.EditMessage(42, id, "list v2"); err != nil || ft.edits[id] != "list v2" {
		t.Fatalf("edit failed: %v (%q)", err, ft.edits[id])
	}
	if err := ft.UnpinMessage(42, id); err != nil || ft.pinned[id] {
		t.Fatalf("unpin failed: %v", err)
	}
}

// The button variants carry LinkButtons through in order; an empty slice is
// a legal "no buttons" send.
func TestTransportLinkButtons(t *testing.T) {
	ft := newFakeTransport()

	want := []LinkButton{
		{Label: "📄 first", URL: "https://notion.so/aaaa1111"},
		{Label: "📄 second", URL: "https://notion.so/bbbb2222"},
	}
	id, err := ft.SendMessageIDButtons(42, 0, "home", want)
	if err != nil || id == 0 {
		t.Fatalf("SendMessageIDButtons = (%d, %v)", id, err)
	}
	if err := ft.EditMessageButtons(42, id, "home v2", want[:1]); err != nil {
		t.Fatal(err)
	}
	if err := ft.NotifyButtons(42, 0, "delta", nil); err != nil {
		t.Fatal(err)
	}

	if len(ft.buttons) != 3 {
		t.Fatalf("buttons calls = %d, want 3", len(ft.buttons))
	}
	if len(ft.buttons[0]) != 2 || ft.buttons[0][0] != want[0] || ft.buttons[0][1] != want[1] {
		t.Errorf("send buttons = %+v, want order preserved", ft.buttons[0])
	}
	if len(ft.buttons[1]) != 1 || ft.buttons[1][0] != want[0] {
		t.Errorf("edit buttons = %+v", ft.buttons[1])
	}
	if len(ft.buttons[2]) != 0 {
		t.Errorf("notify buttons = %+v, want none", ft.buttons[2])
	}
}

// [artifact type="document"] markers become DocumentAttachments: path-based
// (the file stays where it lives — never archived), marker stripped from the
// text, caption carried through.
func TestParseArtifactsDocument(t *testing.T) {
	b := &Bridge{}
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	if err := os.WriteFile(path, []byte("# a doc"), 0o644); err != nil {
		t.Fatal(err)
	}

	var photos []Photo
	var videos []Video
	var docs []DocumentAttachment
	resp := "Here is the doc. [artifact type=\"document\" path=\"" + path + "\" caption=\"current draft\"] Enjoy."
	clean := b.parseArtifacts(resp, &photos, &videos, &docs)

	if len(docs) != 1 || docs[0].Path != path || docs[0].Caption != "current draft" {
		t.Fatalf("docs = %+v", docs)
	}
	if len(photos) != 0 || len(videos) != 0 {
		t.Errorf("document artifact leaked into photos/videos")
	}
	if strings.Contains(clean, "[artifact") {
		t.Errorf("marker not stripped: %q", clean)
	}
	// NOT archived: the file must still exist at its original path when the
	// transport sends it later.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("document was moved/archived: %v", err)
	}
}

// A document marker pointing at a missing file degrades to a visible note,
// same as unreadable images/videos.
func TestParseArtifactsDocumentMissingFile(t *testing.T) {
	b := &Bridge{}
	var photos []Photo
	var videos []Video
	var docs []DocumentAttachment
	clean := b.parseArtifacts(`[artifact type="document" path="/nonexistent/doc.md"]`, &photos, &videos, &docs)
	if len(docs) != 0 {
		t.Errorf("unreadable document collected: %+v", docs)
	}
	if !strings.Contains(clean, "failed to read document") {
		t.Errorf("no visible degradation note: %q", clean)
	}
}
