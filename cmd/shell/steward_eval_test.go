package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func TestOpenSnapshotReadOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "has #hash ünï")
	os.MkdirAll(dir, 0o755)
	src := filepath.Join(dir, "shell.db")
	live, err := store.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	live.CreateProject(store.Project{Slug: "garden", Title: "Garden", ChatID: -100200300})
	if _, err := openSnapshot(src, src); err == nil {
		t.Fatal("a snapshot onto the live db was allowed")
	}
	snap, err := openSnapshot(src, filepath.Join(t.TempDir(), "copy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	if ps, _ := snap.ListProjects(0); len(ps) != 1 {
		t.Fatalf("snapshot has %d projects, want 1", len(ps))
	}
	entries, _ := os.ReadDir(filepath.Dir(dir))
	if len(entries) != 1 {
		t.Fatalf("stray files next to the live db's dir: %v", entries)
	}
}
