// Notion REST client (P3 Wave C, docs/PLAN-PROJECT-WORKSPACE.md). Raw REST
// against api.notion.com — no SDK dependency; only the operations the renderer
// and the Wave D comment loop need, nothing more. The NotionAPI interface exists
// so everything above this file is tested against a fake: the sandbox blocks
// httptest listeners, so the client itself is unit-tested at the
// request-building layer via an injected RoundTripper.
package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	notionBaseURL = "https://api.notion.com/v1"
	// notionVersion pins the API contract. Deliberately the SAME version the
	// rest of the repo speaks (skills/notion, the retired MCP header): one
	// Notion dialect everywhere. It is a currently-supported stable version;
	// bump it repo-wide or not at all.
	notionVersion = "2022-06-28"
	// notionTokenEnv is the token contract shared with skills/notion: the
	// daemon environment provides it (secret store export or plain env). An
	// empty token disables rendering — a no-op with one WARN, never an error
	// on the write path.
	notionTokenEnv = "NOTION_TOKEN"
	// notionMinSpacing paces requests under Notion's ~3 rps limit.
	notionMinSpacing = 350 * time.Millisecond
	// notionAppendChunk is Notion's max children per append call.
	notionAppendChunk = 100
)

// NotionAPI is the narrow surface the page renderer (and later the poller)
// consumes. Tests implement it with a fake; NotionClient is the real thing.
type NotionAPI interface {
	// Enabled reports whether calls can succeed at all (a token is present).
	Enabled() bool
	// CreatePage creates an empty page under a parent page, returning its id.
	CreatePage(ctx context.Context, parentPageID, title, iconEmoji string) (string, error)
	// AppendBlocks appends children to a block (or page), optionally AFTER an
	// existing child block id (empty = at the end). Returns the created ids,
	// in order.
	AppendBlocks(ctx context.Context, blockID string, blocks []NotionBlock, after string) ([]string, error)
	// UpdateBlock replaces an existing block's rich text (same block type).
	UpdateBlock(ctx context.Context, blockID string, b NotionBlock) error
	// DeleteBlock archives (soft-deletes) a block.
	DeleteBlock(ctx context.Context, blockID string) error
	// GetBlockChildren lists a block's (or page's) direct children.
	GetBlockChildren(ctx context.Context, blockID string) ([]NotionBlockRef, error)
	// GetPageLastEdited returns the page's last_edited_time.
	GetPageLastEdited(ctx context.Context, pageID string) (time.Time, error)
	// ListComments returns one page of open comments on a block or page
	// (Notion's listing is NOT recursive: a page id returns page-level
	// comments only, never comments anchored to child blocks). Empty cursor
	// starts at the beginning; a non-empty next cursor means more pages.
	ListComments(ctx context.Context, blockOrPageID, cursor string) (comments []NotionComment, nextCursor string, err error)
	// CreateComment replies INTO an existing discussion thread. Page-level
	// thread creation is deliberately absent — the loop only ever answers.
	CreateComment(ctx context.Context, discussionID string, rich []NotionRichText) (commentID string, err error)
	// Me returns the integration's own bot user id (GET /users/me) — how the
	// poller tells its replies apart from human comments. Callers cache it.
	Me(ctx context.Context) (string, error)
}

// NotionComment is one comment in a discussion thread on a page or block.
type NotionComment struct {
	ID           string
	DiscussionID string
	ParentID     string // parent block id, or the page id for page-level comments
	Plain        string // concatenated plain_text of the rich text
	CreatedByID  string // Notion user id of the author
	CreatedTime  time.Time
}

// NotionRichText is one styled run of text. The converter is deliberately
// minimal: bold and links only, everything else renders plain (plan C2).
type NotionRichText struct {
	Text string
	Bold bool
	Link string // URL; empty = no link
}

// NotionBlock is one Notion block of a supported type.
type NotionBlock struct {
	Type string // paragraph | heading_1 | heading_2 | heading_3 | bulleted_list_item
	Rich []NotionRichText
}

// notionTextLimit is Notion's max characters per rich-text item; longer runs
// are split across items at marshal time.
const notionTextLimit = 2000

// richTextJSON renders the API shape for one run, splitting over-long text.
func richTextJSON(r NotionRichText) []map[string]any {
	runes := []rune(r.Text)
	var out []map[string]any
	for len(runes) > 0 {
		n := len(runes)
		if n > notionTextLimit {
			n = notionTextLimit
		}
		text := map[string]any{"content": string(runes[:n])}
		if r.Link != "" {
			text["link"] = map[string]any{"url": r.Link}
		}
		item := map[string]any{"type": "text", "text": text}
		if r.Bold {
			item["annotations"] = map[string]any{"bold": true}
		}
		out = append(out, item)
		runes = runes[n:]
	}
	return out
}

// blockJSON renders the API shape for one block.
func blockJSON(b NotionBlock) map[string]any {
	rich := []map[string]any{}
	for _, r := range b.Rich {
		rich = append(rich, richTextJSON(r)...)
	}
	return map[string]any{
		"object": "block",
		"type":   b.Type,
		b.Type:   map[string]any{"rich_text": rich},
	}
}

// NotionBlockRef identifies one existing block on a page, with its rich text
// when the type carries one — what the Wave D inverse converter reads back.
type NotionBlockRef struct {
	ID   string
	Type string
	Rich []NotionRichText
}

// NotionAPIError is a non-2xx Notion response. Status is kept so the renderer
// can distinguish "the block map no longer matches the page" (404/410 or a
// validation 400 — trigger the one-shot rebuild) from transient failures.
type NotionAPIError struct {
	Status int
	Body   string
}

func (e *NotionAPIError) Error() string {
	return fmt.Sprintf("notion API: HTTP %d: %s", e.Status, e.Body)
}

// IsNotionConflict reports whether err means the client's picture of the page
// no longer matches reality (deleted/archived blocks, bad ids) — the renderer
// falls back to a full rebuild exactly once on these.
func IsNotionConflict(err error) bool {
	var apiErr *NotionAPIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Status == http.StatusBadRequest ||
		apiErr.Status == http.StatusNotFound ||
		apiErr.Status == http.StatusGone ||
		apiErr.Status == http.StatusConflict
}

// NotionClient is the real NotionAPI over HTTP. All calls serialize through
// one mutex with a minimum inter-call spacing — a single-flight pace that
// keeps a burst of renders under Notion's rate limit. The token is read from
// the environment per call, matching the skills/notion contract.
type NotionClient struct {
	httpc   *http.Client
	baseURL string
	token   func() string

	mu       sync.Mutex
	lastCall time.Time
	spacing  time.Duration
}

// NewNotionClient builds the production client.
func NewNotionClient() *NotionClient {
	return &NotionClient{
		httpc:   &http.Client{Timeout: 30 * time.Second},
		baseURL: notionBaseURL,
		token:   func() string { return os.Getenv(notionTokenEnv) },
		spacing: notionMinSpacing,
	}
}

// Enabled reports whether a token is present.
func (c *NotionClient) Enabled() bool { return c.token() != "" }

// do performs one paced request. body is marshaled when non-nil; the response
// body is decoded into out when non-nil.
func (c *NotionClient) do(ctx context.Context, method, path string, body, out any) error {
	tok := c.token()
	if tok == "" {
		return fmt.Errorf("notion: %s not set", notionTokenEnv)
	}

	// Single-flight + pacing: hold the lock across the whole call so
	// concurrent callers serialize, and space call STARTS by c.spacing.
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.spacing - time.Since(c.lastCall); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.lastCall = time.Now()

	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("notion: marshal %s %s: %w", method, path, err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Notion-Version", notionVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("notion: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("notion: read %s %s: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := string(data)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &NotionAPIError{Status: resp.StatusCode, Body: msg}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("notion: decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

// CreatePage creates an empty page under parentPageID. Blocks are appended
// separately — the create-page response does not return children ids, and the
// block map needs every id.
func (c *NotionClient) CreatePage(ctx context.Context, parentPageID, title, iconEmoji string) (string, error) {
	body := map[string]any{
		"parent": map[string]any{"page_id": parentPageID},
		"properties": map[string]any{
			"title": map[string]any{
				"title": []map[string]any{
					{"type": "text", "text": map[string]any{"content": title}},
				},
			},
		},
	}
	if iconEmoji != "" {
		body["icon"] = map[string]any{"type": "emoji", "emoji": iconEmoji}
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/pages", body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("notion: create page returned no id")
	}
	return out.ID, nil
}

// AppendBlocks appends children under blockID, after `after` when non-empty.
// Chunked at Notion's 100-children limit; each chunk chains `after` off the
// previous chunk's last created id so order is preserved mid-page.
func (c *NotionClient) AppendBlocks(ctx context.Context, blockID string, blocks []NotionBlock, after string) ([]string, error) {
	var ids []string
	for start := 0; start < len(blocks); start += notionAppendChunk {
		end := start + notionAppendChunk
		if end > len(blocks) {
			end = len(blocks)
		}
		children := make([]map[string]any, 0, end-start)
		for _, b := range blocks[start:end] {
			children = append(children, blockJSON(b))
		}
		body := map[string]any{"children": children}
		if after != "" {
			body["after"] = after
		}
		var out struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if err := c.do(ctx, http.MethodPatch, "/blocks/"+blockID+"/children", body, &out); err != nil {
			return ids, err
		}
		for _, r := range out.Results {
			ids = append(ids, r.ID)
		}
		if n := len(out.Results); n > 0 {
			after = out.Results[n-1].ID
		}
	}
	return ids, nil
}

// UpdateBlock replaces blockID's rich text in place (type must match).
func (c *NotionClient) UpdateBlock(ctx context.Context, blockID string, b NotionBlock) error {
	rich := []map[string]any{}
	for _, r := range b.Rich {
		rich = append(rich, richTextJSON(r)...)
	}
	body := map[string]any{b.Type: map[string]any{"rich_text": rich}}
	return c.do(ctx, http.MethodPatch, "/blocks/"+blockID, body, nil)
}

// DeleteBlock archives a block.
func (c *NotionClient) DeleteBlock(ctx context.Context, blockID string) error {
	return c.do(ctx, http.MethodDelete, "/blocks/"+blockID, nil, nil)
}

// notionRichItem is the wire shape of one rich-text run on the read side.
type notionRichItem struct {
	PlainText   string  `json:"plain_text"`
	Href        *string `json:"href"`
	Annotations struct {
		Bold bool `json:"bold"`
	} `json:"annotations"`
}

// richFromWire converts read-side rich text to the converter's shape.
func richFromWire(items []notionRichItem) []NotionRichText {
	var out []NotionRichText
	for _, it := range items {
		r := NotionRichText{Text: it.PlainText, Bold: it.Annotations.Bold}
		if it.Href != nil {
			r.Link = *it.Href
		}
		out = append(out, r)
	}
	return out
}

// GetBlockChildren lists direct children of blockID, following pagination.
// Rich text is decoded for the block types the converter speaks; unknown
// types come back with their Type set and no rich text.
func (c *NotionClient) GetBlockChildren(ctx context.Context, blockID string) ([]NotionBlockRef, error) {
	var refs []NotionBlockRef
	cursor := ""
	for {
		path := "/blocks/" + blockID + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + cursor
		}
		var out struct {
			Results    []json.RawMessage `json:"results"`
			HasMore    bool              `json:"has_more"`
			NextCursor string            `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
			return refs, err
		}
		for _, raw := range out.Results {
			var meta struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &meta); err != nil {
				continue
			}
			ref := NotionBlockRef{ID: meta.ID, Type: meta.Type}
			// The block's content lives under a key named after its type:
			// {"type": "paragraph", "paragraph": {"rich_text": [...]}}.
			var byKey map[string]json.RawMessage
			if json.Unmarshal(raw, &byKey) == nil {
				if body, ok := byKey[meta.Type]; ok {
					var content struct {
						Rich []notionRichItem `json:"rich_text"`
					}
					if json.Unmarshal(body, &content) == nil {
						ref.Rich = richFromWire(content.Rich)
					}
				}
			}
			refs = append(refs, ref)
		}
		if !out.HasMore || out.NextCursor == "" {
			return refs, nil
		}
		cursor = out.NextCursor
	}
}

// GetPageLastEdited returns a page's last_edited_time — the poll loop's
// short-circuit (Wave D).
func (c *NotionClient) GetPageLastEdited(ctx context.Context, pageID string) (time.Time, error) {
	var out struct {
		LastEdited string `json:"last_edited_time"`
	}
	if err := c.do(ctx, http.MethodGet, "/pages/"+pageID, nil, &out); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, out.LastEdited)
	if err != nil {
		return time.Time{}, fmt.Errorf("notion: bad last_edited_time %q: %w", out.LastEdited, err)
	}
	return t, nil
}

// ListComments returns ONE page of open comments on a block or page. The
// cursor rides in the signature (unlike GetBlockChildren) because the Wave D
// poller owns the loop — it sweeps many blocks and wants one place to bound
// the work.
func (c *NotionClient) ListComments(ctx context.Context, blockOrPageID, cursor string) ([]NotionComment, string, error) {
	path := "/comments?block_id=" + blockOrPageID + "&page_size=100"
	if cursor != "" {
		path += "&start_cursor=" + cursor
	}
	var out struct {
		Results []struct {
			ID           string `json:"id"`
			DiscussionID string `json:"discussion_id"`
			Parent       struct {
				Type    string `json:"type"`
				PageID  string `json:"page_id"`
				BlockID string `json:"block_id"`
			} `json:"parent"`
			CreatedTime string `json:"created_time"`
			CreatedBy   struct {
				ID string `json:"id"`
			} `json:"created_by"`
			Rich []notionRichItem `json:"rich_text"`
		} `json:"results"`
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, "", err
	}
	comments := make([]NotionComment, 0, len(out.Results))
	for _, r := range out.Results {
		cm := NotionComment{
			ID:           r.ID,
			DiscussionID: r.DiscussionID,
			ParentID:     r.Parent.BlockID,
			CreatedByID:  r.CreatedBy.ID,
		}
		if cm.ParentID == "" {
			cm.ParentID = r.Parent.PageID
		}
		if t, err := time.Parse(time.RFC3339, r.CreatedTime); err == nil {
			cm.CreatedTime = t
		}
		var plain strings.Builder
		for _, run := range r.Rich {
			plain.WriteString(run.PlainText)
		}
		cm.Plain = plain.String()
		comments = append(comments, cm)
	}
	next := ""
	if out.HasMore {
		next = out.NextCursor
	}
	return comments, next, nil
}

// CreateComment posts a reply into an existing discussion thread.
func (c *NotionClient) CreateComment(ctx context.Context, discussionID string, rich []NotionRichText) (string, error) {
	wire := []map[string]any{}
	for _, r := range rich {
		wire = append(wire, richTextJSON(r)...)
	}
	body := map[string]any{
		"discussion_id": discussionID,
		"rich_text":     wire,
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, "/comments", body, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("notion: create comment returned no id")
	}
	return out.ID, nil
}

// Me returns the integration's own bot user id.
func (c *NotionClient) Me(ctx context.Context) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodGet, "/users/me", nil, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("notion: users/me returned no id")
	}
	return out.ID, nil
}

// notionPageURL builds https://notion.so/<id-no-dashes> from a page id.
func notionPageURL(pageID string) string {
	return "https://notion.so/" + strings.ReplaceAll(pageID, "-", "")
}
