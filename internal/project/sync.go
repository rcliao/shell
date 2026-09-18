// Notion page lifecycle + surgical re-render (P3 Wave C, plan C3): a
// project's canonical doc mirrors to ONE Notion page, created once and then
// updated section by section via the persisted block map. All of this runs
// daemon-side off the family turn path — the render consumer leases
// project.render tasks; nothing here is called synchronously from RPC or a
// chat turn.
package project

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/rcliao/shell/internal/store"
)

// pageFooter is appended once at page creation: tells readers the page is
// machine-maintained and that comments are the feedback channel (Wave D).
const pageFooter = "🤖 這份文件由 agent 維護 — 直接在這裡留言就會處理"

// BlockMap is the persisted section → block-id map (projects.block_map).
// Hashes make the surgical diff possible without refetching the page: a
// section whose current content hash matches its stored hash is untouched.
type BlockMap struct {
	Sections map[string]BlockMapSection `json:"sections"`
	// Order preserves doc order — needed to place a NEW section after its
	// preceding section's last block.
	Order  []string `json:"order,omitempty"`
	Footer string   `json:"footer,omitempty"` // footer paragraph block id
	// Adopted marks a page a human made (see adopt.go): watched for comments,
	// never rendered or reconciled.
	Adopted bool `json:"adopted,omitempty"`
}

// BlockMapSection records one rendered section.
type BlockMapSection struct {
	Hash   string   `json:"hash"`
	Blocks []string `json:"blocks"`
}

// ParseBlockMap decodes projects.block_map. Empty/invalid input returns an
// empty map — the "never rendered by us" state.
func ParseBlockMap(raw string) BlockMap {
	var bm BlockMap
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &bm)
	}
	if bm.Sections == nil {
		bm.Sections = map[string]BlockMapSection{}
	}
	// Fail closed. json.Unmarshal fills what it can and reports a type error
	// for the rest, so a mangled "adopted" field would leave a map that looks
	// rendered — and the renderer would then delete every block of a human's
	// page as a "removed section". The reserved section is the second witness.
	if _, ok := bm.Sections[adoptedSection]; ok {
		bm.Adopted = true
	}
	return bm
}

// SectionForBlock returns the `## ` title of the section containing blockID,
// or "" when the block is unknown or in the preamble — the comment-anchor
// resolution for Wave D revision prompts.
func (bm BlockMap) SectionForBlock(blockID string) string {
	if blockID == "" {
		return ""
	}
	for title, sec := range bm.Sections {
		for _, id := range sec.Blocks {
			if id == blockID {
				if title == preambleSection {
					return ""
				}
				return title
			}
		}
	}
	return ""
}

// Rendered reports whether this map records sections WE rendered — the guard
// that keeps the renderer off pages it did not create (pre-P3 export_refs:
// human pages, or the health-log DATABASE id, which is not even a page).
func (bm BlockMap) Rendered() bool { return len(bm.Sections) > 0 }

func (bm BlockMap) encode() string {
	data, err := json.Marshal(bm)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// NotionPageURL renders the public URL for a project's mirrored page —
// https://notion.so/<id-no-dashes> — but ONLY when export_kind=notion and the
// block map records sections we rendered. A bare export_ref may be a database
// id or a human page whose URL shape we must not guess.
func NotionPageURL(p store.Project) string {
	if p.ExportKind != "notion" || p.ExportRef == "" {
		return ""
	}
	if !ParseBlockMap(p.BlockMap).Rendered() {
		return ""
	}
	return notionPageURL(p.ExportRef)
}

// SyncStore is the slice of the store the renderer needs.
type SyncStore interface {
	UpdateProjectFields(slug string, u store.ProjectFieldUpdate) error
}

// Renderer owns the doc → Notion mirror for all projects. Warnings that
// describe standing configuration (no token, no parent page) fire once per
// project per process, not once per render.
type Renderer struct {
	api    NotionAPI
	parent string // config parent page id; empty = page creation skipped

	mu     sync.Mutex
	warned map[string]bool
}

// NewRenderer builds a renderer over the given API and configured parent.
func NewRenderer(api NotionAPI, parentPageID string) *Renderer {
	return &Renderer{api: api, parent: parentPageID, warned: map[string]bool{}}
}

// warnOnce logs one WARN per (slug, reason) per process.
func (r *Renderer) warnOnce(slug, msg string, args ...any) {
	r.mu.Lock()
	key := slug + "|" + msg
	seen := r.warned[key]
	r.warned[key] = true
	r.mu.Unlock()
	if !seen {
		slog.Warn(msg, append([]any{"slug", slug}, args...)...)
	}
}

// SyncProjectPage brings the project's Notion page in line with docContent.
//
//   - No export_ref (kind empty or notion): create the page under the config
//     parent, full render, persist export_kind/export_ref/block_map.
//   - export_ref WITH a rendered block map: surgical re-render — only
//     sections whose content hash changed are touched.
//   - export_ref WITHOUT a rendered map: SKIP. We never mutate pages (or
//     databases) we did not render.
//
// Unconfigured states (no token, no parent page) no-op with one WARN — never
// an error: rendering is a mirror, and the doc write it mirrors has already
// succeeded.
func (r *Renderer) SyncProjectPage(ctx context.Context, st SyncStore, p *store.Project, docContent string) error {
	if p.ExportKind != "" && p.ExportKind != "notion" {
		slog.Debug("notion render: non-notion export kind, skipping", "slug", p.Slug, "kind", p.ExportKind)
		return nil
	}
	if r.api == nil || !r.api.Enabled() {
		r.warnOnce(p.Slug, "notion render: NOTION_TOKEN not set, skipping render")
		return nil
	}

	if p.ExportRef == "" {
		if r.parent == "" {
			r.warnOnce(p.Slug, "notion render: no parent page configured (notion.project_parent_page_id), skipping page creation")
			return nil
		}
		return r.createAndRender(ctx, st, p, docContent)
	}

	bm := ParseBlockMap(p.BlockMap)
	if !bm.Rendered() {
		// Pre-P3 binding or a page/database a human created: not ours to touch.
		slog.Info("notion render: export_ref without a block map — leaving the external doc alone",
			"slug", p.Slug, "export_ref", p.ExportRef)
		return nil
	}

	if bm.Adopted {
		// A human's page. Rendering would archive every block we cannot
		// express (tables, checkboxes, ...). Never — not even via rebuild.
		slog.Info("notion render: adopted page is watch-only, not rendering", "slug", p.Slug)
		return nil
	}

	err := r.surgicalRender(ctx, st, p, bm, docContent)
	if err != nil && IsNotionConflict(err) {
		// The map disagrees with the page (blocks deleted/moved by hand, stale
		// ids). Fall back ONCE: archive everything we mapped, re-render in
		// full, rebuild the map.
		slog.Warn("notion render: block map out of sync with page, rebuilding",
			"slug", p.Slug, "error", err)
		return r.rebuild(ctx, st, p, bm, docContent)
	}
	return err
}

// createAndRender creates the page and renders every section.
func (r *Renderer) createAndRender(ctx context.Context, st SyncStore, p *store.Project, doc string) error {
	title, sections := ParseDoc(doc)
	if title == "" {
		title = p.Title
	}
	pageID, err := r.api.CreatePage(ctx, r.parent, title, p.Emoji)
	if err != nil {
		return fmt.Errorf("create page for %q: %w", p.Slug, err)
	}

	bm := BlockMap{Sections: map[string]BlockMapSection{}}
	// Persist the page id IMMEDIATELY: if a section append fails halfway, the
	// next render must update THIS page, not create a sibling.
	kind := "notion"
	if err := st.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{
		ExportKind: &kind, ExportRef: &pageID,
	}); err != nil {
		return fmt.Errorf("persist page id for %q: %w", p.Slug, err)
	}
	p.ExportKind, p.ExportRef = kind, pageID

	if err := r.renderAll(ctx, pageID, sections, &bm); err != nil {
		perr := r.persistMap(st, p, bm)
		return errFirst(fmt.Errorf("render %q: %w", p.Slug, err), perr)
	}
	if err := r.persistMap(st, p, bm); err != nil {
		return err
	}
	slog.Info("notion render: page created", "slug", p.Slug, "page_id", pageID,
		"sections", len(sections), "url", notionPageURL(pageID))
	return nil
}

// renderAll appends every section (in order) then the footer, filling bm.
func (r *Renderer) renderAll(ctx context.Context, pageID string, sections []DocSection, bm *BlockMap) error {
	bm.Order = bm.Order[:0]
	for _, sec := range sections {
		ids, err := r.api.AppendBlocks(ctx, pageID, sec.Blocks, "")
		if err != nil {
			return fmt.Errorf("append section %q: %w", sec.Title, err)
		}
		bm.Sections[sec.Title] = BlockMapSection{Hash: sec.Hash, Blocks: ids}
		bm.Order = append(bm.Order, sec.Title)
	}
	footer := NotionBlock{Type: "paragraph", Rich: []NotionRichText{{Text: pageFooter}}}
	ids, err := r.api.AppendBlocks(ctx, pageID, []NotionBlock{footer}, "")
	if err != nil {
		return fmt.Errorf("append footer: %w", err)
	}
	if len(ids) > 0 {
		bm.Footer = ids[0]
	}
	return nil
}

// surgicalRender diffs the doc's sections against the stored map and touches
// only what changed. Never a full-page rewrite when the diff is scoped.
//
// Placement: a CHANGED section's fresh blocks are appended after its own old
// last block (then the old blocks are deleted), so it keeps its position
// without depending on neighbors. A NEW section is appended after the nearest
// preceding section's last block; a new FIRST section falls back to the page
// end (Notion cannot insert before the first child).
func (r *Renderer) surgicalRender(ctx context.Context, st SyncStore, p *store.Project, bm BlockMap, doc string) error {
	_, sections := ParseDoc(doc)
	changed := 0

	// Apply, persisting the map on every exit so a partial failure retries
	// from an accurate picture instead of double-appending.
	apply := func() error {
		current := map[string]bool{}
		for _, sec := range sections {
			current[sec.Title] = true
		}

		// Removed sections: archive their blocks.
		for title, entry := range bm.Sections {
			if current[title] {
				continue
			}
			for _, id := range entry.Blocks {
				if err := r.api.DeleteBlock(ctx, id); err != nil {
					return fmt.Errorf("delete removed section %q: %w", title, err)
				}
			}
			delete(bm.Sections, title)
			changed++
		}

		for i, sec := range sections {
			old, exists := bm.Sections[sec.Title]
			if exists && old.Hash == sec.Hash {
				continue
			}
			after := ""
			if exists && len(old.Blocks) > 0 {
				after = old.Blocks[len(old.Blocks)-1]
			} else {
				// New section: after the preceding (current-doc order) section's
				// last rendered block.
				for j := i - 1; j >= 0; j-- {
					if prev, ok := bm.Sections[sections[j].Title]; ok && len(prev.Blocks) > 0 {
						after = prev.Blocks[len(prev.Blocks)-1]
						break
					}
				}
			}
			ids, err := r.api.AppendBlocks(ctx, p.ExportRef, sec.Blocks, after)
			if err != nil {
				return fmt.Errorf("append section %q: %w", sec.Title, err)
			}
			if exists {
				for _, id := range old.Blocks {
					if err := r.api.DeleteBlock(ctx, id); err != nil {
						// A block another render already archived is gone for our
						// purposes — deleting it again must not read as a map
						// conflict (rapid successive doc-writes hit this).
						if isAlreadyArchived(err) {
							continue
						}
						return fmt.Errorf("delete stale blocks of %q: %w", sec.Title, err)
					}
				}
			}
			bm.Sections[sec.Title] = BlockMapSection{Hash: sec.Hash, Blocks: ids}
			changed++
		}
		return nil
	}

	err := apply()
	bm.Order = bm.Order[:0]
	for _, sec := range sections {
		if _, ok := bm.Sections[sec.Title]; ok {
			bm.Order = append(bm.Order, sec.Title)
		}
	}
	perr := r.persistMap(st, p, bm)
	if err == nil && perr == nil && changed > 0 {
		slog.Info("notion render: surgical update", "slug", p.Slug, "sections_touched", changed)
	}
	return errFirst(err, perr)
}

// rebuild is the one-shot fallback when the map no longer matches the page:
// best-effort archive of everything mapped, then a full re-render at the page
// end and a fresh map.
func (r *Renderer) rebuild(ctx context.Context, st SyncStore, p *store.Project, bm BlockMap, doc string) error {
	for _, entry := range bm.Sections {
		for _, id := range entry.Blocks {
			if err := r.api.DeleteBlock(ctx, id); err != nil {
				slog.Debug("notion render: rebuild archive skipped a block", "slug", p.Slug, "block", id, "error", err)
			}
		}
	}
	if bm.Footer != "" {
		if err := r.api.DeleteBlock(ctx, bm.Footer); err != nil {
			slog.Debug("notion render: rebuild archive skipped footer", "slug", p.Slug, "error", err)
		}
	}

	_, sections := ParseDoc(doc)
	fresh := BlockMap{Sections: map[string]BlockMapSection{}}
	if err := r.renderAll(ctx, p.ExportRef, sections, &fresh); err != nil {
		perr := r.persistMap(st, p, fresh)
		return errFirst(fmt.Errorf("rebuild %q: %w", p.Slug, err), perr)
	}
	if err := r.persistMap(st, p, fresh); err != nil {
		return err
	}
	slog.Warn("notion render: full rebuild completed", "slug", p.Slug, "sections", len(sections))
	return nil
}

// persistMap writes the block map back to the project row.
func (r *Renderer) persistMap(st SyncStore, p *store.Project, bm BlockMap) error {
	encoded := bm.encode()
	if err := st.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{BlockMap: &encoded}); err != nil {
		return fmt.Errorf("persist block map for %q: %w", p.Slug, err)
	}
	p.BlockMap = encoded
	return nil
}

// errFirst returns the first non-nil error.
func errFirst(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
