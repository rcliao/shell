package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two agents' workspaces, one post: the first moves its doc in and owns it,
// the second keeps its own doc as notes, and both read one doc after.
func TestLinkShared(t *testing.T) {
	root, wsA, wsB := t.TempDir(), t.TempDir(), t.TempDir()
	const post = 900000000000000011

	dirA, err := EnsureDocRepo(wsA, "trip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScaffoldDoc(dirA, "Trip", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteDoc(dirA, "# Trip\n\n## 決定\n\nA's decision\n", ""); err != nil {
		t.Fatal(err)
	}
	dirB, _ := EnsureDocRepo(wsB, "trip")
	ScaffoldDoc(dirB, "Trip", "")
	WriteDoc(dirB, "# Trip\n\n## 決定\n\nB's hotel note\n", "")

	owner, err := LinkShared(root, wsA, "trip", "a", post, true)
	if err != nil || !owner {
		t.Fatalf("first link: owner=%v err=%v", owner, err)
	}
	owner, err = LinkShared(root, wsB, "trip", "b", post, false)
	if err != nil || owner {
		t.Fatalf("second link: owner=%v err=%v", owner, err)
	}
	// Idempotent.
	if owner, err := LinkShared(root, wsA, "trip", "a", post, true); err != nil || !owner {
		t.Fatalf("relink: %v %v", owner, err)
	}
	a, _ := ManagedDocDir(wsA, "trip")
	b, _ := ManagedDocDir(wsB, "trip")
	docA, _ := ReadDoc(a)
	docB, _ := ReadDoc(b)
	if docA != docB || !strings.Contains(docA, "A's decision") {
		t.Fatalf("not one doc: %q vs %q", docA, docB)
	}
	notes, err := os.ReadFile(filepath.Join(SharedDir(root, post), NotesFile("b")))
	if err != nil || !strings.Contains(string(notes), "B's hotel note") {
		t.Fatalf("b's notes not kept: %q %v", notes, err)
	}
	if Owner(a) != "a" || MayRunAs(b, "b") || !MayRunAs(b, "a") {
		t.Fatalf("owner = %q", Owner(a))
	}
	// A later agent whose doc is only the scaffold leaves no notes.
	wsC := t.TempDir()
	dirC, _ := EnsureDocRepo(wsC, "trip")
	ScaffoldDoc(dirC, "Trip", "")
	LinkShared(root, wsC, "trip", "c", post, false)
	if _, err := os.Stat(filepath.Join(SharedDir(root, post), NotesFile("c"))); err == nil {
		t.Fatal("a scaffold-only doc left notes")
	}
}

// A write to a shared doc needs a read at the current rev.
func TestCheckFresh(t *testing.T) {
	root, ws := t.TempDir(), t.TempDir()
	dir, _ := EnsureDocRepo(ws, "trip")
	rev, _ := ScaffoldDoc(dir, "Trip", "")
	if err := CheckFresh(dir); err != nil {
		t.Fatalf("unshared doc refused: %v", err)
	}
	LinkShared(root, ws, "trip", "a", 900000000000000012, false)
	dir, _ = ManagedDocDir(ws, "trip")
	if err := CheckFresh(dir); !errors.Is(err, ErrStaleWrite) {
		t.Fatalf("no read: %v", err)
	}
	NoteSeen(dir, rev)
	if err := CheckFresh(dir); err != nil {
		t.Fatalf("fresh read refused: %v", err)
	}
	WriteDoc(dir, "# Trip\n\nother agent\n", "") // the other agent writes
	if err := CheckFresh(dir); !errors.Is(err, ErrStaleWrite) || !strings.Contains(err.Error(), "changed since you read it") {
		t.Fatalf("stale write allowed: %v", err)
	}
}

// Ownership follows research: whichever daemon links first, the agent that
// runs research ends up owning, and research never goes unowned.
func TestOwnershipFollowsResearch(t *testing.T) {
	root, wsA, wsB := t.TempDir(), t.TempDir(), t.TempDir()
	const post = 900000000000000031
	// The agent WITHOUT research links first.
	if owner, _ := LinkShared(root, wsB, "trip", "b", post, false); !owner {
		t.Fatal("first linker should own for now")
	}
	sd := SharedDir(root, post)
	if !MayResearch(sd, "a") || !MayResearch(sd, "b") {
		t.Fatal("with an owner that has no research, nobody may be blocked")
	}
	if owner, _ := LinkShared(root, wsA, "trip", "a", post, true); !owner {
		t.Fatal("the agent with research should take over")
	}
	if Owner(sd) != "a" || !OwnerHasResearch(sd) || MayResearch(sd, "b") || !MayResearch(sd, "a") {
		t.Fatalf("owner=%q research=%v", Owner(sd), OwnerHasResearch(sd))
	}
	// b relinking (restart) does not take it back.
	if owner, _ := LinkShared(root, wsB, "trip", "b", post, false); owner {
		t.Fatal("b took ownership back")
	}
}
