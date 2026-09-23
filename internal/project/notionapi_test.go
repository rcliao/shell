package project

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeTransport implements http.RoundTripper so the client is exercised
// without any listener — the sandbox blocks httptest, and the request
// building is what needs testing anyway.
type fakeTransport struct {
	reqs    []capturedReq
	replies []fakeReply
}

type capturedReq struct {
	method string
	path   string // path?query
	header http.Header
	body   map[string]any
}

type fakeReply struct {
	status int
	body   string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cap := capturedReq{method: req.Method, path: req.URL.Path, header: req.Header}
	if req.URL.RawQuery != "" {
		cap.path += "?" + req.URL.RawQuery
	}
	if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		if len(data) > 0 {
			_ = json.Unmarshal(data, &cap.body)
		}
	}
	f.reqs = append(f.reqs, cap)

	reply := fakeReply{status: 200, body: "{}"}
	if len(f.replies) > 0 {
		reply = f.replies[0]
		f.replies = f.replies[1:]
	}
	return &http.Response{
		StatusCode: reply.status,
		Body:       io.NopCloser(strings.NewReader(reply.body)),
		Header:     http.Header{},
	}, nil
}

func newFakeClient(ft *fakeTransport) *NotionClient {
	return &NotionClient{
		httpc:   &http.Client{Transport: ft},
		baseURL: "https://api.notion.example/v1",
		token:   func() string { return "secret-test-token" },
		spacing: 0, // no pacing in tests
	}
}

func TestNotionCreatePageRequest(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{{200, `{"id": "page-123"}`}}}
	c := newFakeClient(ft)

	id, err := c.CreatePage(context.Background(), "parent-1", "Demo Doc", "🏠")
	if err != nil {
		t.Fatal(err)
	}
	if id != "page-123" {
		t.Errorf("page id = %q, want page-123", id)
	}

	req := ft.reqs[0]
	if req.method != "POST" || req.path != "/v1/pages" {
		t.Errorf("got %s %s, want POST /v1/pages", req.method, req.path)
	}
	if got := req.header.Get("Authorization"); got != "Bearer secret-test-token" {
		t.Errorf("auth header = %q", got)
	}
	if got := req.header.Get("Notion-Version"); got != notionVersion {
		t.Errorf("version header = %q, want %q", got, notionVersion)
	}
	parent := req.body["parent"].(map[string]any)
	if parent["page_id"] != "parent-1" {
		t.Errorf("parent = %v", parent)
	}
	icon := req.body["icon"].(map[string]any)
	if icon["emoji"] != "🏠" {
		t.Errorf("icon = %v", icon)
	}
	title := req.body["properties"].(map[string]any)["title"].(map[string]any)["title"].([]any)
	text := title[0].(map[string]any)["text"].(map[string]any)
	if text["content"] != "Demo Doc" {
		t.Errorf("title content = %v", text)
	}
}

func TestNotionAppendBlocksAfterAndChunking(t *testing.T) {
	// 101 blocks must split into two calls (100 + 1), the second chained
	// after the first chunk's last created id.
	var replies []fakeReply
	first := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		first = append(first, fmt.Sprintf(`{"id": "b%d"}`, i))
	}
	replies = append(replies,
		fakeReply{200, `{"results": [` + strings.Join(first, ",") + `]}`},
		fakeReply{200, `{"results": [{"id": "b100"}]}`},
	)
	ft := &fakeTransport{replies: replies}
	c := newFakeClient(ft)

	blocks := make([]NotionBlock, 101)
	for i := range blocks {
		blocks[i] = NotionBlock{Type: "paragraph", Rich: []NotionRichText{{Text: "x"}}}
	}
	ids, err := c.AppendBlocks(context.Background(), "page-1", blocks, "anchor-0")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 101 {
		t.Fatalf("got %d ids, want 101", len(ids))
	}
	if len(ft.reqs) != 2 {
		t.Fatalf("got %d requests, want 2", len(ft.reqs))
	}
	if ft.reqs[0].path != "/v1/blocks/page-1/children" || ft.reqs[0].method != "PATCH" {
		t.Errorf("first request: %s %s", ft.reqs[0].method, ft.reqs[0].path)
	}
	if ft.reqs[0].body["after"] != "anchor-0" {
		t.Errorf("first chunk after = %v, want anchor-0", ft.reqs[0].body["after"])
	}
	if ft.reqs[1].body["after"] != "b99" {
		t.Errorf("second chunk after = %v, want b99 (chained)", ft.reqs[1].body["after"])
	}
	if n := len(ft.reqs[0].body["children"].([]any)); n != 100 {
		t.Errorf("first chunk children = %d, want 100", n)
	}
}

func TestNotionAppendOmitsAfterWhenEmpty(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{{200, `{"results": [{"id": "b1"}]}`}}}
	c := newFakeClient(ft)
	if _, err := c.AppendBlocks(context.Background(), "p", []NotionBlock{{Type: "paragraph"}}, ""); err != nil {
		t.Fatal(err)
	}
	if _, has := ft.reqs[0].body["after"]; has {
		t.Error("after key present on end-append; must be omitted")
	}
}

func TestNotionBlockJSONShapes(t *testing.T) {
	b := NotionBlock{Type: "bulleted_list_item", Rich: []NotionRichText{
		{Text: "plain "},
		{Text: "loud", Bold: true},
		{Text: "docs", Link: "https://example.com"},
	}}
	data, err := json.Marshal(blockJSON(b))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		`"type":"bulleted_list_item"`,
		`"annotations":{"bold":true}`,
		`"link":{"url":"https://example.com"}`,
		`"content":"plain "`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("block JSON missing %s in %s", want, s)
		}
	}
}

func TestNotionRichTextSplitsAtLimit(t *testing.T) {
	long := strings.Repeat("字", notionTextLimit+5)
	items := richTextJSON(NotionRichText{Text: long, Bold: true})
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	first := items[0]["text"].(map[string]any)["content"].(string)
	if got := len([]rune(first)); got != notionTextLimit {
		t.Errorf("first chunk = %d runes, want %d", got, notionTextLimit)
	}
	if _, ok := items[1]["annotations"]; !ok {
		t.Error("second chunk lost the bold annotation")
	}
}

func TestNotionDeleteAndUpdateAndGets(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{
		{200, `{}`}, // delete
		{200, `{}`}, // update
		{200, `{"results": [{"id": "c1", "type": "paragraph"}], "has_more": true, "next_cursor": "cur2"}`},
		{200, `{"results": [{"id": "c2", "type": "heading_2"}], "has_more": false}`},
		{200, `{"last_edited_time": "2026-08-17T01:02:03.000Z"}`},
	}}
	c := newFakeClient(ft)
	ctx := context.Background()

	if err := c.DeleteBlock(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateBlock(ctx, "b2", NotionBlock{Type: "paragraph", Rich: []NotionRichText{{Text: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	refs, err := c.GetBlockChildren(ctx, "page-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs[0].ID != "c1" || refs[1].ID != "c2" {
		t.Errorf("children = %+v", refs)
	}
	edited, err := c.GetPageLastEdited(ctx, "page-1")
	if err != nil {
		t.Fatal(err)
	}
	if edited.UTC() != time.Date(2026, 8, 17, 1, 2, 3, 0, time.UTC) {
		t.Errorf("last edited = %v", edited)
	}

	if ft.reqs[0].method != "DELETE" || ft.reqs[0].path != "/v1/blocks/b1" {
		t.Errorf("delete: %s %s", ft.reqs[0].method, ft.reqs[0].path)
	}
	if ft.reqs[1].method != "PATCH" || ft.reqs[1].path != "/v1/blocks/b2" {
		t.Errorf("update: %s %s", ft.reqs[1].method, ft.reqs[1].path)
	}
	if !strings.Contains(ft.reqs[3].path, "start_cursor=cur2") {
		t.Errorf("pagination did not pass cursor: %s", ft.reqs[3].path)
	}
}

func TestNotionErrorsAndConflictDetection(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{{404, `{"message": "not found"}`}}}
	c := newFakeClient(ft)
	err := c.DeleteBlock(context.Background(), "gone")
	if err == nil {
		t.Fatal("want error on 404")
	}
	if !IsNotionConflict(err) {
		t.Errorf("404 should read as conflict, got %v", err)
	}
	if IsNotionConflict(fmt.Errorf("dial tcp: timeout")) {
		t.Error("transport errors must NOT read as conflicts")
	}
	if IsNotionConflict(&NotionAPIError{Status: 500}) {
		t.Error("a 500 must NOT read as a conflict (transient, retry instead)")
	}
}

func TestNotionClientDisabledWithoutToken(t *testing.T) {
	c := NewNotionClient(nil)
	c.token = func() string { return "" }
	if c.Enabled() {
		t.Error("no token must report disabled")
	}
	if err := c.DeleteBlock(context.Background(), "x"); err == nil {
		t.Error("calls without a token must error, not silently no-op at this layer")
	}
}

func TestNotionListCommentsRequestAndPagination(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{
		{200, `{"results": [
			{"id": "c1", "discussion_id": "d1",
			 "parent": {"type": "block_id", "block_id": "blk-9"},
			 "created_time": "2026-08-17T10:00:00.000Z",
			 "created_by": {"id": "user-1"},
			 "rich_text": [{"plain_text": "make it "}, {"plain_text": "cheaper"}]}
		], "has_more": true, "next_cursor": "cur-2"}`},
		{200, `{"results": [
			{"id": "c2", "discussion_id": "d2",
			 "parent": {"type": "page_id", "page_id": "page-1"},
			 "created_time": "2026-08-17T11:00:00.000Z",
			 "created_by": {"id": "bot-1"},
			 "rich_text": []}
		], "has_more": false}`},
	}}
	c := newFakeClient(ft)

	got, next, err := c.ListComments(context.Background(), "page-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "cur-2" {
		t.Errorf("next cursor = %q, want cur-2", next)
	}
	if req := ft.reqs[0]; req.method != "GET" || req.path != "/v1/comments?block_id=page-1&page_size=100" {
		t.Errorf("got %s %s", req.method, req.path)
	}
	if len(got) != 1 {
		t.Fatalf("comments = %d, want 1", len(got))
	}
	cm := got[0]
	if cm.ID != "c1" || cm.DiscussionID != "d1" || cm.ParentID != "blk-9" || cm.CreatedByID != "user-1" {
		t.Errorf("comment = %+v", cm)
	}
	if cm.Plain != "make it cheaper" {
		t.Errorf("plain = %q", cm.Plain)
	}
	if cm.CreatedTime.IsZero() {
		t.Error("created_time not parsed")
	}

	// Follow-up page: cursor rides in the query, page-level parent resolves
	// to the page id.
	got, next, err = c.ListComments(context.Background(), "page-1", "cur-2")
	if err != nil {
		t.Fatal(err)
	}
	if next != "" {
		t.Errorf("final page next = %q, want empty", next)
	}
	if req := ft.reqs[1]; !strings.Contains(req.path, "start_cursor=cur-2") {
		t.Errorf("cursor missing from %s", req.path)
	}
	if got[0].ParentID != "page-1" {
		t.Errorf("page-level parent = %q, want page-1", got[0].ParentID)
	}
}

func TestNotionCreateCommentRequest(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{{200, `{"id": "c9"}`}}}
	c := newFakeClient(ft)

	id, err := c.CreateComment(context.Background(), "d1", []NotionRichText{{Text: "done, added option B"}})
	if err != nil {
		t.Fatal(err)
	}
	if id != "c9" {
		t.Errorf("comment id = %q", id)
	}
	req := ft.reqs[0]
	if req.method != "POST" || req.path != "/v1/comments" {
		t.Errorf("got %s %s, want POST /v1/comments", req.method, req.path)
	}
	if req.body["discussion_id"] != "d1" {
		t.Errorf("discussion_id = %v", req.body["discussion_id"])
	}
	rich := req.body["rich_text"].([]any)
	text := rich[0].(map[string]any)["text"].(map[string]any)
	if text["content"] != "done, added option B" {
		t.Errorf("rich text = %v", text)
	}
}

func TestNotionMe(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{{200, `{"object": "user", "id": "bot-user-1", "type": "bot"}`}}}
	c := newFakeClient(ft)
	id, err := c.Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "bot-user-1" {
		t.Errorf("me = %q", id)
	}
	if req := ft.reqs[0]; req.method != "GET" || req.path != "/v1/users/me" {
		t.Errorf("got %s %s", req.method, req.path)
	}
}

func TestNotionGetBlockChildrenDecodesRichText(t *testing.T) {
	ft := &fakeTransport{replies: []fakeReply{{200, `{"results": [
		{"id": "b1", "type": "bulleted_list_item", "bulleted_list_item": {"rich_text": [
			{"plain_text": "option A", "annotations": {"bold": true}},
			{"plain_text": "link", "href": "https://example.com", "annotations": {}}
		]}},
		{"id": "b2", "type": "table", "table": {}}
	], "has_more": false}`}}}
	c := newFakeClient(ft)

	refs, err := c.GetBlockChildren(context.Background(), "page-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %d, want 2", len(refs))
	}
	if refs[0].Rich[0].Text != "option A" || !refs[0].Rich[0].Bold {
		t.Errorf("rich[0] = %+v", refs[0].Rich[0])
	}
	if refs[0].Rich[1].Link != "https://example.com" {
		t.Errorf("rich[1] = %+v", refs[0].Rich[1])
	}
	if refs[1].Type != "table" || len(refs[1].Rich) != 0 {
		t.Errorf("unknown type ref = %+v", refs[1])
	}
}
