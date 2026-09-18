package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Project is a first-class unit of multi-week research work (P1, docs/
// PLAN-PROJECT-WORKSPACE.md): it binds a living document, the conversation,
// scheduled autonomous research, and memory under one slug. ExportRef holds
// the external doc id (e.g. a Notion page id) so the agent never re-derives
// it — the doc-ID-amnesia fix.
type Project struct {
	ID     int64
	Slug   string
	Title  string
	Emoji  string
	Status string // active | paused | archived

	ChatID          int64
	MessageThreadID int64 // Telegram forum topic ID (0 = DM / main chat)

	DocPath            string // workspace/projects/<slug>/doc.md
	DocRev             string // last rendered commit
	ExportKind         string // e.g. "notion"
	ExportRef          string // external doc id (doc-ID amnesia fix)
	BlockMap           string // JSON: section -> external block id
	HandledDiscussions string // JSON array: comment threads already processed (see HandledDiscussion)
	NotionWatermark    string // last seen Notion page last_edited_time (RFC3339); Wave D poll short-circuit
	NotionPolledAt     *time.Time // last completed comment sweep; quiet projects are polled less often (P3.5)

	Instructions string
	NotifyPolicy string // quiet | announce
	Lang         string

	GhostTag         string // ghost memory tag, default project:<slug>
	ScheduleDedupKey string

	TopicThreadRef      *int64 // nullable FK -> topic_threads (binding, P4)
	ReviewAfter         *time.Time
	LastResearchAt      *time.Time
	LastHumanActivityAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// validProjectStatus reports whether s is one of the three lifecycle states.
func validProjectStatus(s string) bool {
	return s == "active" || s == "paused" || s == "archived"
}

// SlugifyTitle derives a URL-ish slug from a project title: lowercase,
// CJK-safe (letters and digits of any script are kept), every other run of
// characters collapses to a single '-'. Uniqueness against existing rows is
// CreateProject's job, not this function's.
func SlugifyTitle(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// CreateProject inserts a new project row. The slug is derived from the
// title when empty (see SlugifyTitle) and made unique by suffixing -2, -3, …
// against existing rows. GhostTag defaults to project:<slug>; status to
// active; notify_policy to quiet. Returns the stored row (read back).
func (s *Store) CreateProject(p Project) (*Project, error) {
	if p.Title == "" {
		return nil, fmt.Errorf("title required")
	}
	if p.Status == "" {
		p.Status = "active"
	}
	if !validProjectStatus(p.Status) {
		return nil, fmt.Errorf("invalid status %q: must be active, paused, or archived", p.Status)
	}
	if p.NotifyPolicy == "" {
		p.NotifyPolicy = "quiet"
	}
	if p.BlockMap == "" {
		p.BlockMap = "{}"
	}
	if p.HandledDiscussions == "" {
		p.HandledDiscussions = "[]"
	}
	if p.Slug == "" {
		p.Slug = SlugifyTitle(p.Title)
	}
	if p.Slug == "" {
		return nil, fmt.Errorf("cannot derive a slug from title %q", p.Title)
	}

	// Ensure slug uniqueness by suffixing -2, -3, … . Single-writer daemon, so
	// check-then-insert is safe; the UNIQUE constraint backstops it regardless.
	base := p.Slug
	for i := 2; ; i++ {
		existing, err := s.GetProjectBySlug(p.Slug)
		if err != nil {
			return nil, err
		}
		if existing == nil {
			break
		}
		if i > 100 {
			return nil, fmt.Errorf("could not find a free slug for %q", base)
		}
		p.Slug = fmt.Sprintf("%s-%d", base, i)
	}

	if p.GhostTag == "" {
		p.GhostTag = "project:" + p.Slug
	}

	_, err := s.db.Exec(`
		INSERT INTO projects
		  (slug, title, emoji, status, chat_id, message_thread_id,
		   doc_path, doc_rev, export_kind, export_ref, block_map, handled_discussions,
		   notion_watermark, instructions, notify_policy, lang, ghost_tag, schedule_dedup_key,
		   topic_thread_ref, review_after, last_research_at, last_human_activity_at, notion_polled_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Slug, p.Title, p.Emoji, p.Status, p.ChatID, p.MessageThreadID,
		p.DocPath, p.DocRev, p.ExportKind, p.ExportRef, p.BlockMap, p.HandledDiscussions,
		p.NotionWatermark, p.Instructions, p.NotifyPolicy, p.Lang, p.GhostTag, p.ScheduleDedupKey,
		p.TopicThreadRef, p.ReviewAfter, p.LastResearchAt, p.LastHumanActivityAt, p.NotionPolledAt)
	if err != nil {
		return nil, err
	}
	return s.GetProjectBySlug(p.Slug)
}

const projectColumns = `id, slug, title, emoji, status, chat_id, message_thread_id,
	doc_path, doc_rev, export_kind, export_ref, block_map, handled_discussions,
	notion_watermark, instructions, notify_policy, lang, ghost_tag, schedule_dedup_key,
	topic_thread_ref, review_after, last_research_at, last_human_activity_at, notion_polled_at,
	created_at, updated_at`

// scanProject scans one projects row from any row-shaped scanner.
func scanProject(scan func(dest ...any) error) (*Project, error) {
	var p Project
	var topicRef sql.NullInt64
	var reviewAfter, lastResearch, lastHuman, polledAt sql.NullTime
	err := scan(&p.ID, &p.Slug, &p.Title, &p.Emoji, &p.Status, &p.ChatID, &p.MessageThreadID,
		&p.DocPath, &p.DocRev, &p.ExportKind, &p.ExportRef, &p.BlockMap, &p.HandledDiscussions,
		&p.NotionWatermark, &p.Instructions, &p.NotifyPolicy, &p.Lang, &p.GhostTag, &p.ScheduleDedupKey,
		&topicRef, &reviewAfter, &lastResearch, &lastHuman, &polledAt,
		&p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if topicRef.Valid {
		p.TopicThreadRef = &topicRef.Int64
	}
	if reviewAfter.Valid {
		p.ReviewAfter = &reviewAfter.Time
	}
	if lastResearch.Valid {
		p.LastResearchAt = &lastResearch.Time
	}
	if lastHuman.Valid {
		p.LastHumanActivityAt = &lastHuman.Time
	}
	if polledAt.Valid {
		p.NotionPolledAt = &polledAt.Time
	}
	return &p, nil
}

// GetProjectBySlug returns the project with the given slug, or nil if absent.
func (s *Store) GetProjectBySlug(slug string) (*Project, error) {
	row := s.db.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE slug = ?`, slug)
	p, err := scanProject(row.Scan)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ListProjects returns projects for a chat (all statuses), newest first.
// chatID=0 means all chats.
func (s *Store) ListProjects(chatID int64) ([]Project, error) {
	var q string
	var args []any
	if chatID == 0 {
		q = `SELECT ` + projectColumns + ` FROM projects ORDER BY created_at DESC`
	} else {
		q = `SELECT ` + projectColumns + ` FROM projects WHERE chat_id = ? ORDER BY created_at DESC`
		args = []any{chatID}
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// UpdateProjectStatus moves a project between lifecycle states
// (active | paused | archived). Errors on an unknown slug so callers can
// report not-found instead of silently no-oping.
func (s *Store) UpdateProjectStatus(slug, status string) error {
	if !validProjectStatus(status) {
		return fmt.Errorf("invalid status %q: must be active, paused, or archived", status)
	}
	res, err := s.db.Exec(`
		UPDATE projects SET status = ?, updated_at = CURRENT_TIMESTAMP
		WHERE slug = ?`, status, slug)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("project %q does not exist", slug)
	}
	return nil
}

// ProjectFieldUpdate carries targeted column updates for UpdateProjectFields.
// Nil pointers mean "leave unchanged"; a non-nil pointer sets the column
// (including to the zero value). Slug and status are deliberately not here —
// slug is immutable, status goes through UpdateProjectStatus. The chat
// binding IS here: `shell project bind` re-targets a project's chat/thread.
type ProjectFieldUpdate struct {
	Title               *string
	Emoji               *string
	ChatID              *int64
	MessageThreadID     *int64
	DocPath             *string
	DocRev              *string
	ExportKind          *string
	ExportRef           *string
	BlockMap            *string
	HandledDiscussions  *string
	NotionWatermark     *string
	Instructions        *string
	NotifyPolicy        *string
	Lang                *string
	GhostTag            *string
	ScheduleDedupKey    *string
	TopicThreadRef      *int64
	ReviewAfter         *time.Time
	LastResearchAt      *time.Time
	LastHumanActivityAt *time.Time
}

// MarkProjectPolled records a completed Notion comment sweep. Deliberately
// NOT routed through UpdateProjectFields: that bumps updated_at, and a poll
// is bookkeeping, not a change to the project — the home list and staleness
// checks must not see a project as touched because we looked at it.
func (s *Store) MarkProjectPolled(slug string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE projects SET notion_polled_at = ? WHERE slug = ?`, at.UTC(), slug)
	return err
}

// UpdateProjectFields applies the non-nil fields of u to the project row.
// Errors on an unknown slug or when no field is set.
func (s *Store) UpdateProjectFields(slug string, u ProjectFieldUpdate) error {
	var sets []string
	var args []any
	add := func(col string, v any) {
		sets = append(sets, col+" = ?")
		args = append(args, v)
	}
	if u.Title != nil {
		add("title", *u.Title)
	}
	if u.Emoji != nil {
		add("emoji", *u.Emoji)
	}
	if u.ChatID != nil {
		add("chat_id", *u.ChatID)
	}
	if u.MessageThreadID != nil {
		add("message_thread_id", *u.MessageThreadID)
	}
	if u.DocPath != nil {
		add("doc_path", *u.DocPath)
	}
	if u.DocRev != nil {
		add("doc_rev", *u.DocRev)
	}
	if u.ExportKind != nil {
		add("export_kind", *u.ExportKind)
	}
	if u.ExportRef != nil {
		add("export_ref", *u.ExportRef)
	}
	if u.BlockMap != nil {
		add("block_map", *u.BlockMap)
	}
	if u.HandledDiscussions != nil {
		add("handled_discussions", *u.HandledDiscussions)
	}
	if u.NotionWatermark != nil {
		add("notion_watermark", *u.NotionWatermark)
	}
	if u.Instructions != nil {
		add("instructions", *u.Instructions)
	}
	if u.NotifyPolicy != nil {
		add("notify_policy", *u.NotifyPolicy)
	}
	if u.Lang != nil {
		add("lang", *u.Lang)
	}
	if u.GhostTag != nil {
		add("ghost_tag", *u.GhostTag)
	}
	if u.ScheduleDedupKey != nil {
		add("schedule_dedup_key", *u.ScheduleDedupKey)
	}
	if u.TopicThreadRef != nil {
		add("topic_thread_ref", *u.TopicThreadRef)
	}
	if u.ReviewAfter != nil {
		add("review_after", *u.ReviewAfter)
	}
	if u.LastResearchAt != nil {
		add("last_research_at", *u.LastResearchAt)
	}
	if u.LastHumanActivityAt != nil {
		add("last_human_activity_at", *u.LastHumanActivityAt)
	}
	if len(sets) == 0 {
		return fmt.Errorf("no fields to update")
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	args = append(args, slug)
	res, err := s.db.Exec(`UPDATE projects SET `+strings.Join(sets, ", ")+` WHERE slug = ?`, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("project %q does not exist", slug)
	}
	return nil
}

// HandledDiscussion is one processed Notion comment thread, recorded in
// projects.handled_discussions (JSON array). The timestamp exists for the
// pinned home's recent-comment marker; the id is the replay guard.
type HandledDiscussion struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// ParseHandledDiscussions decodes projects.handled_discussions leniently:
// the current shape is an array of {"id","at"} objects, but rows written
// before the timestamp existed may carry plain strings — those parse with a
// zero time. Invalid input returns an empty list, never an error: a broken
// ledger must degrade to "nothing handled yet", not block the poller.
func ParseHandledDiscussions(raw string) []HandledDiscussion {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil
	}
	var out []HandledDiscussion
	for _, item := range items {
		var h HandledDiscussion
		if err := json.Unmarshal(item, &h); err == nil && h.ID != "" {
			out = append(out, h)
			continue
		}
		var id string
		if err := json.Unmarshal(item, &id); err == nil && id != "" {
			out = append(out, HandledDiscussion{ID: id})
		}
	}
	return out
}

// HandledDiscussionSet returns the handled ids as a set for O(1) guards.
func HandledDiscussionSet(raw string) map[string]bool {
	entries := ParseHandledDiscussions(raw)
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		set[e.ID] = true
	}
	return set
}

// AppendHandledDiscussion marks one comment thread as processed. Idempotent:
// an id already present leaves the row untouched. Read-modify-write is safe
// here — the daemon is the single writer, same as CreateProject's slug check.
func (s *Store) AppendHandledDiscussion(slug, discussionID string) error {
	if discussionID == "" {
		return fmt.Errorf("discussion id required")
	}
	p, err := s.GetProjectBySlug(slug)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("project %q does not exist", slug)
	}
	entries := ParseHandledDiscussions(p.HandledDiscussions)
	for _, e := range entries {
		if e.ID == discussionID {
			return nil
		}
	}
	entries = append(entries, HandledDiscussion{ID: discussionID, At: time.Now().UTC()})
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	encoded := string(data)
	return s.UpdateProjectFields(slug, ProjectFieldUpdate{HandledDiscussions: &encoded})
}
