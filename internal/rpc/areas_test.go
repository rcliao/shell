package rpc

import (
	"fmt"
	"net/http"
	"testing"
)

// fakePlaces records what the RPC asked the platform to do.
type fakePlaces struct {
	next     int64
	created  []string // "ref|title|content|tags"
	tags     map[int64][]string
	archived []int64
	fail     bool
}

func (f *fakePlaces) CreateThread(chatID int64, ref, title, content string, tags []string) (int64, error) {
	if f.fail {
		return 0, fmt.Errorf("boom")
	}
	f.next++
	f.created = append(f.created, fmt.Sprintf("%s|%s|%s|%v", ref, title, content, tags))
	return 900000000000000000 + f.next, nil
}
func (f *fakePlaces) SetThreadTags(chatID, threadID int64, ref string, tags []string) error {
	if f.tags == nil {
		f.tags = map[int64][]string{}
	}
	f.tags[threadID] = tags
	return nil
}
func (f *fakePlaces) ArchiveThread(chatID, threadID int64, ref string) error {
	f.archived = append(f.archived, threadID)
	return nil
}

const forumRef = "discord:900000000000000099"

func newAreaServer(t *testing.T) (*Server, *fakePlaces) {
	s, _ := newProjectTestServer(t)
	fp := &fakePlaces{}
	s.places = fp
	code, out := postProject(t, s, map[string]any{"action": "create", "title": "Travel", "slug": "travel",
		"chat_id": -100200300, "kind": "area", "place_ref": forumRef, "lang": "zh"})
	if code != http.StatusOK || out["kind"] != "area" {
		t.Fatalf("area create: %d %v", code, out)
	}
	return s, fp
}

func TestAreaProjectGetsPlaceStageAndClose(t *testing.T) {
	s, fp := newAreaServer(t)

	code, out := postProject(t, s, map[string]any{"action": "create", "title": "Trip", "emoji": "✈️",
		"area": "travel", "stage": "planning", "place": "auto", "content": "summary"})
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, out)
	}
	if out["place_created"] != true || out["message_thread_id"] != float64(900000000000000001) {
		t.Fatalf("place not bound: %v", out)
	}
	if out["chat_id"] != float64(-100200300) || out["lang"] != "zh" {
		t.Fatalf("chat/lang not inherited from the area: %v", out)
	}
	if len(fp.created) != 1 || fp.created[0] != forumRef+"|✈️ Trip|summary|[planning]" {
		t.Fatalf("created = %v", fp.created)
	}
	slug := out["slug"].(string)

	_, got := postProject(t, s, map[string]any{"action": "get", "slug": slug})
	if got["message_thread_id"] != float64(900000000000000001) {
		t.Fatalf("thread not persisted: %v", got)
	}

	code, out = postProject(t, s, map[string]any{"action": "stage", "slug": slug, "stage": "booked"})
	if code != http.StatusOK || out["stage"] != "booked" {
		t.Fatalf("stage: %d %v", code, out)
	}
	if tags := fp.tags[900000000000000001]; len(tags) != 1 || tags[0] != "booked" {
		t.Fatalf("tags = %v", fp.tags)
	}

	code, out = postProject(t, s, map[string]any{"action": "status", "slug": slug, "status": "archived"})
	if code != http.StatusOK || len(fp.archived) != 1 || fp.archived[0] != 900000000000000001 {
		t.Fatalf("archive: %d %v archived=%v", code, out, fp.archived)
	}
}

func TestAreaMoveExistingProjectIntoPost(t *testing.T) {
	s, fp := newAreaServer(t)
	_, out := postProject(t, s, map[string]any{"action": "create", "title": "Old trip", "chat_id": -100200300})
	slug := out["slug"].(string)

	code, out := postProject(t, s, map[string]any{"action": "move", "slug": slug, "area": "travel",
		"stage": "planning", "place": "auto", "content": "doc summary"})
	if code != http.StatusOK || out["area"] != "travel" || out["place_created"] != true {
		t.Fatalf("move: %d %v", code, out)
	}
	if len(fp.created) != 1 {
		t.Fatalf("created = %v", fp.created)
	}
	// A second move with place=auto must not open a second post.
	code, _ = postProject(t, s, map[string]any{"action": "move", "slug": slug, "area": "travel", "place": "auto"})
	if code != http.StatusBadRequest || len(fp.created) != 1 {
		t.Fatalf("second place: %d created=%v", code, fp.created)
	}
}

func TestAreaValidation(t *testing.T) {
	s, fp := newAreaServer(t)
	_, out := postProject(t, s, map[string]any{"action": "create", "title": "Plain", "chat_id": -100200300})
	plain := out["slug"].(string)

	cases := []map[string]any{
		{"action": "create", "title": "X", "chat_id": 1, "place": "auto"},                             // no area
		{"action": "create", "title": "X", "area": "nope"},                                            // unknown area
		{"action": "create", "title": "X", "area": plain},                                             // not an area
		{"action": "create", "title": "X", "chat_id": 1, "kind": "area", "place_ref": "discord:abc"},  // bad ref
		{"action": "create", "title": "X", "chat_id": 1, "place_ref": forumRef},                       // ref on a project
		{"action": "create", "title": "X", "area": "travel", "place": "auto", "chat_id": 7},           // other chat
		{"action": "create", "title": "X", "area": "travel", "place": "auto", "message_thread_id": 5}, // both
		{"action": "create", "title": "X", "chat_id": 1, "kind": "area", "area": "travel"},            // nested
		{"action": "move", "slug": "travel", "area": "travel"},                                        // area into area
	}
	for i, c := range cases {
		if code, out := postProject(t, s, c); code != http.StatusBadRequest {
			t.Errorf("case %d: %d %v", i, code, out)
		}
	}
	if len(fp.created) != 0 {
		t.Fatalf("a rejected request created a place: %v", fp.created)
	}

	// A platform failure keeps the project, unbound, with a warning.
	fp.fail = true
	code, out := postProject(t, s, map[string]any{"action": "create", "title": "Y", "area": "travel", "place": "auto"})
	if code != http.StatusOK || out["place_warning"] == nil || out["message_thread_id"] != float64(0) {
		t.Fatalf("failed place: %d %v", code, out)
	}
}

// A failed place is reported in its own field, so a later emoji or scaffold
// warning cannot hide it.
func TestPlaceWarningNotOverwritten(t *testing.T) {
	s, fp := newAreaServer(t)
	fp.fail = true
	_, out := postProject(t, s, map[string]any{"action": "create", "title": "Z", "emoji": "✈️", "area": "travel", "place": "auto"})
	if out["place_warning"] == nil {
		t.Fatalf("place failure hidden: %v", out)
	}
}
