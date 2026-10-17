package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/store"
)

// runStewardAuto does one auto steward proposal (docs/DESIGN-STEWARD-AUTO.md)
// as a turn in its project's thread and posts the turn's report there. The
// proposal is closed by "auto": done when the turn reported, withdrawn when
// it replied [noop] (on a closer look the step broke a rule or was not
// needed). A failed turn returns its error so the queue retries it; one
// that never runs is withdrawn by the next steward delivery
// (withdrawStaleAuto). A step queued before auto was turned off is
// withdrawn, not run.
func (d projectResearchDeps) runStewardAuto(ctx context.Context, p project.EventPayload) (string, error) {
	sg, err := d.store.GetSuggestion(p.SuggestionID)
	if err != nil {
		return "", fmt.Errorf("load suggestion #%d: %w", p.SuggestionID, err)
	}
	// Proposed too: the queue may lease the task before delivery has
	// marked the row.
	if sg == nil || (sg.Status != store.SuggestionDelivered && sg.Status != store.SuggestionProposed) || store.StewardKind(sg.Evidence) != "auto" {
		return "skipped: not a waiting auto proposal", nil
	}
	if sg.Status == store.SuggestionProposed {
		if err := d.store.MarkSuggestionsDelivered([]int64{sg.ID}); err != nil {
			return "", fmt.Errorf("mark auto #%d delivered: %w", sg.ID, err)
		}
	}
	if !d.stewardAutoOn {
		_ = d.store.DecideSuggestion(sg.ID, store.SuggestionWithdrawn, store.DecidedByAuto, "auto was turned off before it ran")
		return "skipped: auto is off", nil
	}
	proj, err := d.store.GetProjectBySlug(sg.Project)
	if err != nil {
		return "", fmt.Errorf("load project %q: %w", sg.Project, err)
	}
	if proj == nil || proj.Status != "active" {
		_ = d.store.DecideSuggestion(sg.ID, store.SuggestionWithdrawn, store.DecidedByAuto, "project not active")
		return "skipped: project not active", nil
	}
	text, err := d.runProjectTurn(ctx, proj, project.StewardAutoPrompt(sg.ID, *proj, sg.Change, sg.Evidence))
	if err != nil {
		return "", fmt.Errorf("steward auto #%d: %w", sg.ID, err)
	}
	reply := strings.TrimSpace(text)
	if reply == "" || strings.Contains(reply, "[noop]") {
		if err := d.store.DecideSuggestion(sg.ID, store.SuggestionWithdrawn, store.DecidedByAuto, "the turn dropped it on a closer look"); err != nil {
			slog.Warn("steward auto: withdraw failed", "id", sg.ID, "error", err)
		}
		slog.Info("steward auto: dropped by its turn", "id", sg.ID, "slug", proj.Slug)
		return fmt.Sprintf("auto #%d dropped by its turn", sg.ID), nil
	}
	if d.deliver != nil {
		d.deliver(proj.ChatID, proj.MessageThreadID, reply, nil)
	}
	// The lifecycle reaches done only through accepted; the turn may have
	// recorded accepted itself, so only a still-delivered row needs it.
	note := truncateRunes(oneLine(reply), 300)
	if cur, err := d.store.GetSuggestion(sg.ID); err == nil && cur != nil && cur.Status == store.SuggestionDelivered {
		if err := d.store.DecideSuggestion(sg.ID, store.SuggestionAccepted, store.DecidedByAuto, note); err != nil {
			slog.Warn("steward auto: mark accepted failed", "id", sg.ID, "error", err)
		}
	}
	if err := d.store.DecideSuggestion(sg.ID, store.SuggestionDone, store.DecidedByAuto, note); err != nil {
		slog.Warn("steward auto: mark done failed", "id", sg.ID, "error", err)
	}
	slog.Info("steward auto: done", "id", sg.ID, "slug", proj.Slug)
	return fmt.Sprintf("auto #%d done and reported", sg.ID), nil
}

// staleAutoAge is when a queued auto step that has not run is given up: past
// its task's 24 h TTL, with an hour for a turn still running.
const staleAutoAge = 25 * time.Hour

// withdrawStaleAuto withdraws auto proposals still waiting to run after
// staleAutoAge (their task failed every attempt or expired), so they stop
// holding their project. Called on each steward delivery.
func withdrawStaleAuto(st *store.Store, now time.Time) error {
	rows, err := st.StewardSince(now.Add(-store.StewardOpenAge))
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		if store.StewardKind(r.Evidence) != "auto" || r.Status != store.SuggestionDelivered || r.DeliveredAt == nil || now.Sub(*r.DeliveredAt) < staleAutoAge {
			continue
		}
		if err := st.DecideSuggestion(r.ID, store.SuggestionWithdrawn, store.DecidedByAuto, "did not run"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
