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
	areas    map[string][2]int64 // channel name → (channel thread, forum id)
	titles   map[int64]string    // thread → post title (without emoji)
}

func (f *fakePlaces) EnsureAreaPlaces(chatID int64, name, forumName string, tags []string) (int64, string, bool, error) {
	if f.areas == nil {
		f.areas = map[string][2]int64{}
	}
	if a, ok := f.areas[name]; ok {
		return a[0], fmt.Sprintf("discord:%d", a[1]), false, nil
	}
	f.next += 2
	a := [2]int64{800000000000000000 + f.next, 800000000000000001 + f.next}
	f.areas[name] = a
	return a[0], fmt.Sprintf("discord:%d", a[1]), true, nil
}

func (f *fakePlaces) FindThread(chatID int64, ref, title string) (int64, bool, error) {
	for id, t := range f.titles {
		if t == title {
			return id, true, nil
		}
	}
	return 0, false, nil
}

func (f *fakePlaces) ThreadInfo(chatID, threadID int64) (string, string, []string, error) {
	t, ok := f.titles[threadID]
	if !ok {
		return "", "", nil, fmt.Errorf("no thread")
	}
	return "🎮 " + t, forumRef, []string{"planning"}, nil
}

func (f *fakePlaces) CreateThread(chatID int64, ref, title, content string, tags []string) (int64, error) {
	if f.fail {
		return 0, fmt.Errorf("boom")
	}
	f.next++
	f.created = append(f.created, fmt.Sprintf("%s|%s|%s|%v", ref, title, content, tags))
	id := 900000000000000000 + f.next
	if f.titles == nil {
		f.titles = map[int64]string{}
	}
	_, bare := splitLeadingEmoji(title)
	f.titles[id] = bare
	return id, nil
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

// Two agents (two stores, one server): both run the same area create; the
// second reuses the channels, and a repeat on one agent is a no-op.
func TestAreaPlaceNewFindOrCreate(t *testing.T) {
	fp := &fakePlaces{}
	a, _ := newProjectTestServer(t)
	b, _ := newProjectTestServer(t)
	a.places, b.places = fp, fp
	req := map[string]any{"action": "create", "title": "Gaming", "emoji": "🎮", "chat_id": -100200300, "kind": "area", "place": "new"}

	code, out := postProject(t, a, req)
	if code != http.StatusOK || out["places_created"] != true || out["channel_mention"] == nil || out["forum_mention"] == nil {
		t.Fatalf("first agent: %d %v", code, out)
	}
	code, out2 := postProject(t, b, req)
	if code != http.StatusOK || out2["places_created"] != false || out2["message_thread_id"] != out["message_thread_id"] || out2["place_ref"] != out["place_ref"] {
		t.Fatalf("second agent: %d %v", code, out2)
	}
	code, again := postProject(t, a, req)
	if code != http.StatusOK || again["created"] != false || again["slug"] != out["slug"] {
		t.Fatalf("repeat: %d %v", code, again)
	}
	if _, list := postProject(t, a, map[string]any{"action": "list", "chat_id": -100200300}); list["count"] != float64(1) {
		t.Fatalf("repeat made a second row: %v", list)
	}
	if code, _ := postProject(t, a, map[string]any{"action": "create", "title": "X", "chat_id": 1, "place": "new"}); code != http.StatusBadRequest {
		t.Fatal("place=new on a plain project must be refused")
	}
}

// The post is the shared record: a same-title create joins it, and join
// registers an agent's own row for another agent's post.
func TestPostIsSharedRecord(t *testing.T) {
	s, fp := newAreaServer(t)
	_, first := postProject(t, s, map[string]any{"action": "create", "title": "Zelda", "area": "travel", "place": "auto"})
	var postID int64 // from the fake: JSON floats cannot hold a snowflake exactly
	for id := range fp.titles {
		postID = id
	}
	if first["place_created"] != true || postID == 0 {
		t.Fatalf("first post: %v", first)
	}

	other, _ := newProjectTestServer(t)
	other.places = fp
	postProject(t, other, map[string]any{"action": "create", "title": "Travel", "slug": "travel", "chat_id": -100200300, "kind": "area", "place_ref": forumRef})
	code, out := postProject(t, other, map[string]any{"action": "create", "title": "Zelda", "area": "travel", "place": "auto"})
	if code != http.StatusOK || out["place_joined"] != true || out["place_created"] != false || len(fp.created) != 1 {
		t.Fatalf("same title opened another post: %d %v created=%v", code, out, fp.created)
	}

	third, _ := newProjectTestServer(t)
	third.places = fp
	postProject(t, third, map[string]any{"action": "create", "title": "Travel", "slug": "travel", "chat_id": -100200300, "kind": "area", "place_ref": forumRef})
	code, j := postProject(t, third, map[string]any{"action": "join", "chat_id": -100200300, "message_thread_id": postID})
	if code != http.StatusOK || j["joined"] != true || j["title"] != "Zelda" || j["emoji"] != "🎮" || j["area"] != "travel" || j["stage"] != "planning" {
		t.Fatalf("join: %d %v", code, j)
	}
	code, again := postProject(t, third, map[string]any{"action": "join", "chat_id": -100200300, "message_thread_id": postID})
	if code != http.StatusOK || again["joined"] != false {
		t.Fatalf("second join: %d %v", code, again)
	}
	noArea, _ := newProjectTestServer(t)
	noArea.places = fp
	if code, _ := postProject(t, noArea, map[string]any{"action": "join", "chat_id": -100200300, "message_thread_id": postID}); code != http.StatusBadRequest {
		t.Fatal("join without the area must be refused")
	}
}

// A retry is a no-op; a joined post gets no second research schedule.
func TestAreaCreateRetryAndJoinSchedule(t *testing.T) {
	s, fp := newAreaServer(t)
	_, first := postProject(t, s, map[string]any{"action": "create", "title": "Zelda", "area": "travel", "place": "auto"})
	code, again := postProject(t, s, map[string]any{"action": "create", "title": "zelda", "area": "travel", "place": "auto"})
	if code != http.StatusOK || again["created"] != false || again["slug"] != first["slug"] || len(fp.created) != 1 {
		t.Fatalf("retry: %d %v created=%v", code, again, fp.created)
	}
	other, _ := newProjectTestServer(t)
	other.places = fp
	postProject(t, other, map[string]any{"action": "create", "title": "Travel", "slug": "travel", "chat_id": -100200300, "kind": "area", "place_ref": forumRef})
	_, joined := postProject(t, other, map[string]any{"action": "create", "title": "Zelda", "area": "travel", "place": "auto"})
	if joined["place_joined"] != true || joined["cadence"] != "none (joined another agent's post)" {
		t.Fatalf("joined create: %v", joined)
	}
}
