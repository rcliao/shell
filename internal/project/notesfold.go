package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// EventNotesFold asks this agent, once, to fold its own notes into a doc
// that became shared (docs/DESIGN-PROJECT-AREAS.md, part 4): a hint in the
// turn block was not enough — seen live, all notes stayed unfolded.
const EventNotesFold = "notes.fold"

const notesFoldTTL = 24 * time.Hour

// NotesPath is this agent's notes file beside a shared doc, "" when none.
func NotesPath(dir, agent string) string {
	if dir == "" || agent == "" {
		return ""
	}
	p := filepath.Join(dir, NotesFile(agent))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// EnqueueNotesFold queues the fold task for p when this agent has notes to
// fold. Idempotent per day, so a fold the agent did not finish is asked
// again tomorrow and never twice the same day.
func EnqueueNotesFold(q RenderQueue, workspaceDir, agent string, p store.Project, now time.Time) (bool, error) {
	dir, ok := ManagedDocDir(workspaceDir, p.Slug)
	if !ok || NotesPath(dir, agent) == "" {
		return false, nil
	}
	payload, err := json.Marshal(EventPayload{Event: EventNotesFold, Slug: p.Slug, ChatID: p.ChatID, ThreadID: p.MessageThreadID})
	if err != nil {
		return false, err
	}
	exp := now.UTC().Add(notesFoldTTL)
	_, created, err := q.EnqueueTask(store.Task{
		Kind:           EventKind,
		Source:         store.TaskSourceAgent,
		IdempotencyKey: store.DeriveIdempotencyKey(EventKind, EventNotesFold, p.Slug, now.Format("2006-01-02")),
		PartitionKey:   fmt.Sprintf("chat:%d:%d", p.ChatID, p.MessageThreadID), // scheduler.PartitionKey
		Payload:        string(payload),
		ExpiresAt:      &exp,
	})
	return created, err
}

// NotesFoldPrompt is the quiet one-time turn that folds an agent's notes
// into the shared doc.
func NotesFoldPrompt(slug, title, notesPath, notes, doc string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Project notes: %s]\n", slug)
	fmt.Fprintf(&b, "Project: %s\n", title)
	b.WriteString("This project's doc is now shared with the other agent. Before it was shared, you kept your own doc; its content is below as your notes.\n")
	b.WriteString("\nYour notes (" + notesPath + "):\n---\n" + notes)
	if !strings.HasSuffix(notes, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("---\n\nThe shared doc now:\n---\n" + doc)
	if !strings.HasSuffix(doc, "\n") {
		b.WriteString("\n")
	}
	b.WriteString("---\n\nDo this once, quietly. Hard rules:\n")
	b.WriteString("- Fold into the shared doc only what it lacks: a decision, a fact, an open question, a correction. Never re-add what is already there, never drop what the other agent wrote.\n")
	b.WriteString("- Write through the project skill (doc-read, then doc-write with the full revised doc); the printed rev is your receipt.\n")
	b.WriteString("- Then delete the notes file: rm " + notesPath + "\n")
	b.WriteString("- Do not message the chat about this. Reply [noop] when done.\n")
	return b.String()
}
