package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// A shared doc's Notion state lives beside the doc, not in either agent's
// row (docs/DESIGN-PROJECT-WIKI.md): both agents render to ONE page, and
// only the owner polls it. The rows keep the page id as their link.

const (
	bindingFile     = "notion.json"
	bindingLockFile = ".binding.lock" // short read-modify-writes of notion.json
	notionLockFile  = ".notion.lock"  // one render or reconcile of the page at a time
)

// DocBinding is a shared doc's Notion state. BlockMap and Handled hold the
// same JSON as the projects columns they replace.
type DocBinding struct {
	PageID      string     `json:"page_id"`
	BlockMap    string     `json:"block_map,omitempty"`
	RenderedRev string     `json:"rendered_rev,omitempty"`
	RenderedAt  *time.Time `json:"rendered_at,omitempty"`
	Watermark   string     `json:"watermark,omitempty"`
	Handled     string     `json:"handled,omitempty"`
}

// ErrNotionBusy means the other daemon is rendering or reconciling the page.
// The task queue retries.
var ErrNotionBusy = errors.New("notion page busy: the other agent is rendering it")

// LoadBinding reads dir's binding; nil without error when there is none.
func LoadBinding(dir string) (*DocBinding, error) {
	data, err := os.ReadFile(filepath.Join(dir, bindingFile))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var b DocBinding
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("parse %s: %w", bindingFile, err)
	}
	return &b, nil
}

// saveBinding writes dir's binding atomically. Callers hold the binding lock.
func saveBinding(dir string, b DocBinding) error {
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, bindingFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// updateBinding applies fn to dir's binding (empty when there is none) under
// the binding lock and saves it.
func updateBinding(dir string, fn func(*DocBinding)) error {
	unlock, err := lockPath(filepath.Join(mustEval(dir), bindingLockFile))
	if err != nil {
		return err
	}
	defer unlock()
	b, err := LoadBinding(dir)
	if err != nil {
		return err
	}
	if b == nil {
		b = &DocBinding{}
	}
	fn(b)
	return saveBinding(dir, *b)
}

// TryLockNotion takes the page lock without waiting: a render can take
// minutes, and the other daemon's queue worker must not stall behind it.
// Lock order: the page lock, then the doc lock — never the reverse.
func TryLockNotion(dir string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(mustEval(dir), notionLockFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrNotionBusy
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

// RecordRender notes that the page now shows rev.
func RecordRender(dir, rev string) error {
	now := time.Now().UTC()
	return updateBinding(dir, func(b *DocBinding) {
		b.RenderedRev = rev
		b.RenderedAt = &now
	})
}

// BindingState says how the Notion code treats a project.
type BindingState int

const (
	// Unshared: the row holds the Notion state, as before.
	Unshared BindingState = iota
	// SharedBound: the doc's binding holds it (possibly with no page yet,
	// when the owner is about to create one).
	SharedBound
	// SharedUnbound: shared, no binding yet, and this agent is not the
	// owner — the owner seeds it; until then this agent leaves Notion alone.
	SharedUnbound
)

// BindingStore is the store as the Notion code sees it: for a shared doc the
// page's state comes from and goes to the doc's binding; for anything else
// it is the store itself.
type BindingStore struct {
	*store.Store
	WorkspaceDir string
	Agent        string
}

// SharedDocDir returns slug's doc dir when it is a shared doc.
func (b BindingStore) SharedDocDir(slug string) (string, bool) {
	dir, ok := ManagedDocDir(b.WorkspaceDir, slug)
	if !ok || !IsShared(dir) {
		return "", false
	}
	return dir, true
}

// GetProjectBySlug loads a project with its doc's binding overlaid.
func (b BindingStore) GetProjectBySlug(slug string) (*store.Project, error) {
	p, err := b.Store.GetProjectBySlug(slug)
	if err != nil || p == nil {
		return p, err
	}
	if _, err := b.Overlay(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Overlay replaces p's Notion fields with its doc's binding. The owner seeds
// a missing binding from its own row. Any agent whose row names a different
// page is pointed at the binding's page: that row's old page is a duplicate.
func (b BindingStore) Overlay(p *store.Project) (BindingState, error) {
	dir, ok := b.SharedDocDir(p.Slug)
	if !ok || (p.ExportKind != "" && p.ExportKind != "notion") {
		return Unshared, nil
	}
	bd, err := LoadBinding(dir)
	if err != nil {
		return SharedBound, err
	}
	if bd == nil {
		if Owner(dir) != b.Agent {
			return SharedUnbound, nil
		}
		if p.ExportRef != "" && (p.ExportKind == "" || p.ExportKind == "notion") {
			if err := updateBinding(dir, func(nb *DocBinding) {
				if nb.PageID != "" {
					return // seeded meanwhile
				}
				nb.PageID, nb.BlockMap = p.ExportRef, p.BlockMap
				nb.Watermark, nb.Handled = p.NotionWatermark, p.HandledDiscussions
			}); err != nil {
				return SharedBound, fmt.Errorf("seed binding for %q: %w", p.Slug, err)
			}
			slog.Info("notion binding: seeded from row", "slug", p.Slug, "page_id", p.ExportRef)
			if bd, err = LoadBinding(dir); err != nil {
				return SharedBound, err
			}
		} else {
			bd = &DocBinding{}
		}
	}

	if bd.PageID != "" && p.ExportRef != bd.PageID {
		kind, ref := "notion", bd.PageID
		if err := b.Store.UpdateProjectFields(p.Slug, store.ProjectFieldUpdate{ExportKind: &kind, ExportRef: &ref}); err != nil {
			slog.Warn("notion binding: row page id sync failed", "slug", p.Slug, "error", err)
		} else if p.ExportRef != "" {
			slog.Info("notion binding: row now points at the doc's page; its old page is a duplicate",
				"slug", p.Slug, "old_page", p.ExportRef, "page_id", bd.PageID)
		}
	}
	p.ExportRef, p.BlockMap = bd.PageID, bd.BlockMap
	p.NotionWatermark, p.HandledDiscussions = bd.Watermark, bd.Handled
	if bd.PageID != "" {
		p.ExportKind = "notion"
	} else {
		p.ExportKind = ""
	}
	return SharedBound, nil
}

// UpdateProjectFields sends a shared doc's Notion fields to its binding and
// everything else to the row. The page id goes to both: it is the row's link.
func (b BindingStore) UpdateProjectFields(slug string, u store.ProjectFieldUpdate) error {
	dir, ok := b.SharedDocDir(slug)
	if !ok {
		return b.Store.UpdateProjectFields(slug, u)
	}
	if u.ExportRef != nil || u.BlockMap != nil || u.NotionWatermark != nil || u.HandledDiscussions != nil {
		if err := updateBinding(dir, func(bd *DocBinding) {
			if u.ExportRef != nil {
				bd.PageID = *u.ExportRef
			}
			if u.BlockMap != nil {
				bd.BlockMap = *u.BlockMap
			}
			if u.NotionWatermark != nil {
				bd.Watermark = *u.NotionWatermark
			}
			if u.HandledDiscussions != nil {
				bd.Handled = *u.HandledDiscussions
			}
		}); err != nil {
			return fmt.Errorf("update binding for %q: %w", slug, err)
		}
	}
	u.BlockMap, u.NotionWatermark, u.HandledDiscussions = nil, nil, nil
	if u == (store.ProjectFieldUpdate{}) {
		return nil
	}
	return b.Store.UpdateProjectFields(slug, u)
}

// AppendHandledDiscussion records a processed comment thread where the
// project's Notion state lives.
func (b BindingStore) AppendHandledDiscussion(slug, discussionID string) error {
	dir, ok := b.SharedDocDir(slug)
	if !ok {
		return b.Store.AppendHandledDiscussion(slug, discussionID)
	}
	if discussionID == "" {
		return fmt.Errorf("discussion id required")
	}
	var merr error
	err := updateBinding(dir, func(bd *DocBinding) {
		entries := store.ParseHandledDiscussions(bd.Handled)
		for _, e := range entries {
			if e.ID == discussionID {
				return
			}
		}
		entries = append(entries, store.HandledDiscussion{ID: discussionID, At: time.Now().UTC()})
		data, err := json.Marshal(entries)
		if err != nil {
			merr = err
			return
		}
		bd.Handled = string(data)
	})
	if err != nil {
		return err
	}
	return merr
}
