package event

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/store"
)

func TestIngestSpool(t *testing.T) {
	settleTime = 0 // files in this test are complete when written
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	if n, err := IngestSpool(dir, st); err != nil || n != 0 {
		t.Fatalf("no inbox: n=%d err=%v", n, err)
	}
	inbox := filepath.Join(dir, "inbox")
	os.MkdirAll(inbox, 0o755)
	write := func(name, body string) { os.WriteFile(filepath.Join(inbox, name), []byte(body), 0o644) }
	write("a.json", `{"source":"gmail","kind":"email.received","dedup_id":"m1","summary":"Flight confirmation","ref":"m1","occurred_at":"2026-09-26T08:00:00Z"}`)
	write("b.json", `{"source":"gmail","kind":"email.received","dedup_id":"m1","summary":"duplicate of m1"}`)
	write("c.json", `not json`)
	write("d.json", `{"source":"gmail","kind":"email.received","summary":"no dedup id"}`)
	write("note.txt", `ignored`)

	n, err := IngestSpool(dir, st)
	if err != nil || n != 1 {
		t.Fatalf("ingest: n=%d err=%v", n, err)
	}
	for name, where := range map[string]string{"a.json": "done", "b.json": "done", "c.json": "rejected", "d.json": "rejected"} {
		if _, err := os.Stat(filepath.Join(dir, where, name)); err != nil {
			t.Errorf("%s should be in %s/: %v", name, where, err)
		}
	}
	if _, err := os.Stat(filepath.Join(inbox, "note.txt")); err != nil {
		t.Error("non-JSON files are left alone")
	}
	evs, _ := st.ListEvents(nil, 0)
	if len(evs) != 1 || evs[0].Summary != "Flight confirmation" || evs[0].OccurredAt.Year() != 2026 {
		t.Fatalf("events = %+v", evs)
	}
	if n, _ := IngestSpool(dir, st); n != 0 {
		t.Error("a second pass finds nothing new")
	}
}

func TestIngestSpoolWaitsForSettledFiles(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "inbox"), 0o755)
	os.WriteFile(filepath.Join(dir, "inbox", "fresh.json"), []byte(`{"source":"s","kind":"k","dedup_id":"1","summary":"x"}`), 0o644)
	settleTime = time.Hour
	defer func() { settleTime = 2 * time.Second }()
	if n, _ := IngestSpool(dir, st); n != 0 {
		t.Error("a file that may still be being written must wait")
	}
	if _, err := os.Stat(filepath.Join(dir, "inbox", "fresh.json")); err != nil {
		t.Error("it stays in the inbox until settled")
	}
}
