// Package event ingests external events from an agent's spool directory
// (docs/DESIGN-HEARTBEAT-AGENDA-EVENTS.md). Producers live outside shell —
// typically a cron script (a Gmail or calendar poll) — and drop one JSON file
// per event into <agent dir>/events/inbox/. Ingestion is idempotent: the
// store deduplicates on (source, dedup_id), and each file moves to done/ or,
// if it cannot be read as an event, to rejected/.
package event

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/store"
)

// File is the spool file format.
type File struct {
	Source     string `json:"source"`
	Kind       string `json:"kind"`
	DedupID    string `json:"dedup_id"`
	Summary    string `json:"summary"`
	Ref        string `json:"ref"`
	OccurredAt string `json:"occurred_at"` // RFC 3339; optional
	ChatID     int64  `json:"chat_id"`     // optional: the chat it concerns
}

// settleTime is how long a spool file must be unchanged before it is read.
var settleTime = 2 * time.Second

// maxSummaryRunes keeps an event a one-liner: detail is fetched, not stored.
const maxSummaryRunes = 300

// SpoolDir returns an agent's spool root.
func SpoolDir(agentDir string) string { return filepath.Join(agentDir, "events") }

// IngestSpool reads every *.json in <dir>/inbox, records it, and moves it to
// done/ (or rejected/). Returns how many new events were recorded.
func IngestSpool(dir string, st *store.Store) (int, error) {
	inbox := filepath.Join(dir, "inbox")
	entries, err := os.ReadDir(inbox)
	if os.IsNotExist(err) {
		return 0, nil // no spool yet: nothing to do
	}
	if err != nil {
		return 0, err
	}
	// Only settled files: a producer should write <name>.tmp and rename it
	// to <name>.json, but one that writes in place must not be read halfway.
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if info, err := e.Info(); err != nil || time.Since(info.ModTime()) < settleTime {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	added := 0
	for _, name := range names {
		path := filepath.Join(inbox, name)
		ev, perr := parse(path)
		dest := "done"
		if perr != nil {
			slog.Warn("events: rejected spool file", "file", name, "error", perr)
			dest = "rejected"
		} else if _, created, err := st.AddEvent(ev); err != nil {
			slog.Warn("events: rejected spool file", "file", name, "error", err)
			dest = "rejected"
		} else if created {
			added++
		}
		if err := move(path, filepath.Join(dir, dest, name)); err != nil {
			return added, err
		}
	}
	return added, nil
}

func parse(path string) (store.Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return store.Event{}, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return store.Event{}, fmt.Errorf("not JSON: %w", err)
	}
	summary := strings.Join(strings.Fields(f.Summary), " ")
	if r := []rune(summary); len(r) > maxSummaryRunes {
		summary = string(r[:maxSummaryRunes]) + "…"
	}
	e := store.Event{Source: strings.TrimSpace(f.Source), Kind: strings.TrimSpace(f.Kind), DedupID: strings.TrimSpace(f.DedupID),
		Summary: summary, Ref: strings.TrimSpace(f.Ref), ChatID: f.ChatID}
	if f.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339, f.OccurredAt)
		if err != nil {
			return store.Event{}, fmt.Errorf("occurred_at: %w", err)
		}
		e.OccurredAt = t
	}
	if e.Source == "" || e.Kind == "" || e.DedupID == "" || e.Summary == "" {
		return store.Event{}, fmt.Errorf("source, kind, dedup_id and summary are required")
	}
	return e, nil
}

func move(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}
