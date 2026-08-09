package store

import (
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func verifyStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, dir
}

// ledger writes a file and records it with its true digest, the way archiving
// does.
func ledger(t *testing.T, b *Store, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, size, err := FileDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordMedia(-100200300, 0, 1, path, "", sum, size); err != nil {
		t.Fatal(err)
	}
	return path
}

// The failure this exists for: a file that was truncated or rewritten after
// archiving. Nothing noticed before, because nothing recorded what the bytes
// were supposed to be.
func TestVerifyMediaDetectsAlteredBytes(t *testing.T) {
	b, dir := verifyStore(t)
	path := ledger(t, b, dir, "photo.jpg", "the original bytes")

	if err := os.WriteFile(path, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	v, err := b.VerifyMedia()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(v) != 1 {
		t.Fatalf("verdicts = %d, want 1", len(v))
	}
	if v[0].Status != MediaCorrupt {
		t.Fatalf("status = %q, want corrupt — an altered file went unreported", v[0].Status)
	}
	if v[0].Detail == "" {
		t.Error("corrupt verdict carried no detail; nothing to act on")
	}
}

// An intact file must pass. A checker that flags everything is as useless as
// one that flags nothing, and is likelier to be ignored.
func TestVerifyMediaPassesIntactFiles(t *testing.T) {
	b, dir := verifyStore(t)
	ledger(t, b, dir, "photo.jpg", "the original bytes")

	v, err := b.VerifyMedia()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v[0].Status != MediaOK {
		t.Fatalf("status = %q (%s), want ok", v[0].Status, v[0].Detail)
	}
}

// A deleted file is missing, not corrupt. Different cause, different fix: one
// means restore it, the other means the bytes on disk are lying.
func TestVerifyMediaSeparatesMissingFromCorrupt(t *testing.T) {
	b, dir := verifyStore(t)
	path := ledger(t, b, dir, "gone.jpg", "bytes")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	v, err := b.VerifyMedia()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v[0].Status != MediaMissing {
		t.Fatalf("status = %q, want missing", v[0].Status)
	}
}

// Rows archived before digests existed cannot be judged. Reporting the back
// catalogue as corrupt on the day this shipped would train everyone to ignore
// the output.
func TestVerifyMediaReportsPreDigestRowsAsUnverifiable(t *testing.T) {
	b, dir := verifyStore(t)
	path := filepath.Join(dir, "legacy.jpg")
	if err := os.WriteFile(path, []byte("archived long ago"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.RecordMedia(-100200300, 0, 7, path, "", "", 0); err != nil {
		t.Fatal(err)
	}

	v, err := b.VerifyMedia()
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if v[0].Status != MediaUnverifiable {
		t.Fatalf("status = %q, want unverifiable — a legacy row must not read as corrupt", v[0].Status)
	}
}
