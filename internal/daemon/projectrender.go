package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// The project.render consumer (P3 Wave C, docs/PLAN-PROJECT-WORKSPACE.md):
// doc writes enqueue project.render{slug}; THIS handler leases it and mirrors
// the doc to its Notion page via the block-map renderer. All Notion I/O
// happens here, on a queue worker — never on the family turn path. The
// "render:<slug>" partition serializes renders per project without ever
// contending with chat turns.

// projectRenderTimeout bounds one render: a full first render of a large doc
// is dozens of paced (~350ms) API calls, nowhere near this.
const projectRenderTimeout = 5 * time.Minute

// projectRenderDeps carries what the render consumer needs.
type projectRenderDeps struct {
	store        *store.Store
	workspaceDir string
	agentName    string
	renderer     *project.Renderer
}

// wireProjectRender registers the project.render handler. Unconditional at
// startup for the same reason as wireProjectEvents: an unregistered kind
// fails loudly at lease time.
func wireProjectRender(sched *scheduler.Scheduler, deps projectRenderDeps) {
	sched.RegisterHandler(project.RenderKind, deps.handleProjectRender)
}

// readDocAtHead reads the doc and the rev it is at. For a shared doc both are
// read under the doc lock, so the rev recorded as rendered is the content
// rendered: a newer rev with older content would let a page-edit reconcile
// roll back the other agent's write.
func readDocAtHead(dir string, shared bool) (doc, rev string, err error) {
	if shared {
		unlock, err := project.LockDoc(dir)
		if err != nil {
			return "", "", err
		}
		defer unlock()
	}
	if rev, err = project.Head(dir); err != nil {
		return "", "", err
	}
	doc, err = project.ReadDoc(dir)
	return doc, rev, err
}

func (d projectRenderDeps) handleProjectRender(ctx context.Context, t scheduler.LeasedTask) (string, error) {
	p, err := project.DecodeRenderPayload(t.Payload)
	if err != nil {
		// Undecodable bytes stay undecodable; the attempts cap bounds it.
		return "", err
	}

	bs := project.BindingStore{Store: d.store, WorkspaceDir: d.workspaceDir, Agent: d.agentName}
	proj, err := d.store.GetProjectBySlug(p.Slug)
	if err != nil {
		return "", fmt.Errorf("load project %q: %w", p.Slug, err)
	}
	if proj == nil {
		slog.Warn("project render: project missing, skipping", "slug", p.Slug)
		return "skipped: project not found", nil
	}

	// Only MANAGED docs render — an external doc_path has no repo here to
	// read, and its export_ref (if any) is not a page we made.
	dir, ok := project.ManagedDocDir(d.workspaceDir, proj.Slug)
	if !ok {
		slog.Info("project render: no managed doc, skipping", "slug", p.Slug)
		return "skipped: no managed doc", nil
	}

	// A shared doc renders to the one page in its binding, whichever agent
	// wrote (docs/DESIGN-PROJECT-WIKI.md).
	state, err := bs.Overlay(proj)
	if err != nil {
		return "", fmt.Errorf("load binding for %q: %w", p.Slug, err)
	}
	if state == project.SharedUnbound {
		slog.Info("project render: shared doc has no binding yet, the owner seeds it", "slug", p.Slug, "owner", project.Owner(dir))
		return "skipped: waiting for the owner's binding", nil
	}
	shared := state == project.SharedBound
	if shared {
		unlock, err := project.TryLockNotion(dir)
		if err != nil {
			return "", fmt.Errorf("render %q: %w", p.Slug, err)
		}
		defer unlock()
	}

	doc, rev, err := readDocAtHead(dir, shared)
	if err != nil {
		return "", fmt.Errorf("read doc for %q: %w", p.Slug, err)
	}

	renderCtx, cancel := context.WithTimeout(ctx, projectRenderTimeout)
	defer cancel()
	if err := d.renderer.SyncProjectPage(renderCtx, bs, proj, doc); err != nil {
		return "", fmt.Errorf("render %q: %w", p.Slug, err)
	}
	if shared && proj.ExportRef != "" {
		if err := project.RecordRender(dir, rev); err != nil {
			slog.Warn("project render: record rendered rev failed", "slug", p.Slug, "error", err)
		}
	}
	if url := project.NotionPageURL(*proj); url != "" {
		return "rendered: " + url, nil
	}
	// Unconfigured (no token / no parent) or unmanaged export — the renderer
	// already logged why.
	return "skipped: notion export not configured", nil
}
