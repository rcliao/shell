package bridge

import (
	"context"
	"log/slog"
	"time"

	"github.com/rcliao/shell/internal/decide"
	"github.com/rcliao/shell/internal/route"
	"github.com/rcliao/shell/internal/store"
)

// Live routing in shadow (R0, docs/DESIGN-ROUTER-AND-SUGGESTIONS.md). The
// Jev shadow already asks which_project on every real turn; its answer is
// handed here, the sticky rule is applied, and one route_decisions row per
// backend is logged — the Jev answer and the local keyword baseline, so the
// report can say what the hosted model adds. Nothing reads these rows on the
// turn path.

// defaultStickyThreshold is used when route.sticky_threshold is unset.
const defaultStickyThreshold = 0.6

// SetRouteStickyThreshold sets the sticky rule's confidence threshold.
func (b *Bridge) SetRouteStickyThreshold(th float64) { b.routeSticky = th }

func (b *Bridge) stickyThreshold() float64 {
	if b.routeSticky > 0 {
		return b.routeSticky
	}
	return defaultStickyThreshold
}

// logLiveRoute is the decide.Shadow OnWhichProject hook. The v1 answer logs
// the jev backend plus the keyword baseline; the v2 answer logs jev-v2.
func (b *Bridge) logLiveRoute(t decide.Turn, question, choice string, confidence float64, latency time.Duration) {
	if b.store == nil {
		return
	}
	hash := store.TextHash(t.Message)
	now := time.Now()
	record := func(backend string, c route.Choice) {
		prev, err := b.store.LastRouteLane("live", backend, t.ChatID, t.ThreadID)
		if err != nil {
			slog.Warn("route: previous lane lookup failed", "error", err)
		}
		lane, sticky := route.Decide(prev, c, b.stickyThreshold())
		if err := b.store.LogRouteDecision(store.RouteDecision{Source: "live", ChatID: t.ChatID, ThreadID: t.ThreadID,
			MsgID: t.MsgID, MsgAt: now, TextHash: hash, Backend: backend, LanePrev: prev, Choice: c.Lane,
			Confidence: c.Confidence, Lane: lane, Sticky: sticky, LatencyMS: c.Latency.Milliseconds()}); err != nil {
			slog.Warn("route: decision log failed", "backend", backend, "error", err)
		}
	}
	if question == "which_project_v2" {
		record("jev-v2", route.Choice{Lane: route.LaneFromJev(choice), Confidence: confidence, Latency: latency})
		return
	}
	record("jev", route.Choice{Lane: route.LaneFromJev(choice), Confidence: confidence, Latency: latency})
	kw, _ := route.Keyword{}.Choose(context.Background(), route.Input{ChatKind: t.ChatKind, ThreadID: t.ThreadID,
		Text: t.Message, Candidates: route.FromProjects(t.Projects)})
	record("keyword", kw)
}
