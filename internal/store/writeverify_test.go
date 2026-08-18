package store

import (
	"path/filepath"
	"testing"
	"time"
)

// The P2 project_id column is nullable: project doc writes carry the id,
// every other verification row stays NULL — and both shapes must insert.
func TestLogWriteVerificationWithAndWithoutProject(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "wv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	pid := int64(7)
	if err := st.LogWriteVerification(WriteVerification{
		ChatID: 42, Classification: "verified", Claimed: true, WriteOK: true,
		ToolNames: "project.doc-write", Source: "rpc", ProjectID: &pid,
	}); err != nil {
		t.Fatalf("insert with project_id: %v", err)
	}
	if err := st.LogWriteVerification(WriteVerification{
		ChatID: 42, Classification: "verbal_save", Claimed: true,
	}); err != nil {
		t.Fatalf("insert without project_id: %v", err)
	}

	sum, err := st.GetWriteHygieneSummary(42, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Verified != 1 || sum.VerbalSave != 1 {
		t.Errorf("summary = %+v, want 1 verified + 1 verbal_save", sum)
	}
}
