package project

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// The project.render contract (P3 Wave C): every successful doc write — the
// doc-write RPC and the research consumer — enqueues one render task; the
// daemon's render consumer leases it and mirrors the doc to Notion. The shape
// lives here so producers and the consumer agree by construction, mirroring
// event.go.

// RenderKind is the task-queue kind for Notion render work.
const RenderKind = "project.render"

// renderTaskTTL bounds staleness. Generous: a render reads the CURRENT doc,
// so running late is still correct — but a task older than a day (daemon down
// through it) is better re-triggered by the next write than replayed.
const renderTaskTTL = 24 * time.Hour

// RenderPayload is the project.render task payload. Just the slug: the
// consumer reads the doc's current content at run time, so a coalesced or
// late render is automatically up to date.
type RenderPayload struct {
	Slug string `json:"slug"`
}

// RenderPartition serializes renders per project without ever blocking chat
// turns — deliberately NOT the (chat, thread) partition.
func RenderPartition(slug string) string { return "render:" + slug }

// RenderQueue is the enqueue surface (satisfied by *store.Store).
type RenderQueue interface {
	EnqueueTask(t store.Task) (int64, bool, error)
}

// EnqueueRender registers a render for the project at docRev. Idempotent on
// (kind, slug, docRev): the same rev never renders twice, however many
// triggers fire for it (RPC doc-write + research consumer both do).
func EnqueueRender(q RenderQueue, slug, docRev string) (created bool, err error) {
	if slug == "" || docRev == "" {
		return false, fmt.Errorf("render enqueue needs slug and doc rev")
	}
	payload, err := json.Marshal(RenderPayload{Slug: slug})
	if err != nil {
		return false, err
	}
	exp := time.Now().UTC().Add(renderTaskTTL)
	_, created, err = q.EnqueueTask(store.Task{
		Kind:           RenderKind,
		Source:         store.TaskSourceAgent,
		IdempotencyKey: store.DeriveIdempotencyKey(RenderKind, slug, docRev),
		PartitionKey:   RenderPartition(slug),
		Payload:        string(payload),
		ExpiresAt:      &exp,
	})
	return created, err
}

// DecodeRenderPayload parses a project.render task payload.
func DecodeRenderPayload(payload string) (RenderPayload, error) {
	var p RenderPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return p, fmt.Errorf("undecodable project.render payload: %w", err)
	}
	if p.Slug == "" {
		return p, fmt.Errorf("project.render payload needs slug")
	}
	return p, nil
}
