package rpc

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rcliao/shell/internal/project"
	"github.com/rcliao/shell/internal/scheduler"
	"github.com/rcliao/shell/internal/store"
)

// B1: project create self-registers an event-mode research schedule
// (dedup_key = project:<slug>); archive/pause disables it and re-activation
// re-enables it.

func newScheduledProjectServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "shell.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Server{
		store:    st,
		timezone: "UTC",
		cronParse: func(expr string) (interface{ Next(time.Time) time.Time }, error) {
			return scheduler.ParseCron(expr)
		},
	}, st
}

func TestProjectCreateRegistersResearchSchedule(t *testing.T) {
	s, st := newScheduledProjectServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "Housing Search", "chat_id": 42,
		"doc_path": "workspace/external.md", // external doc: skip scaffold, keep the test hermetic
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}
	if out["cadence"] != "weekly" {
		t.Errorf("cadence = %v, want default weekly", out["cadence"])
	}

	sc, err := st.FindScheduleByDedupKey("project:housing-search")
	if err != nil {
		t.Fatal(err)
	}
	if sc == nil {
		t.Fatal("research schedule was not registered")
	}
	if !sc.Enabled || sc.Mode != scheduler.ModeEvent || sc.Type != "cron" {
		t.Errorf("schedule = mode %q type %q enabled %t, want enabled event cron", sc.Mode, sc.Type, sc.Enabled)
	}
	// First project in a fresh store is id 1 → the :20 slot.
	if want := "20 9 * * 1"; sc.Schedule != want {
		t.Errorf("expr = %q, want staggered %q", sc.Schedule, want)
	}

	// The envelope must parse as an event message carrying the project's binding.
	kind, payload, err := scheduler.ParseEventMessage(sc.Message)
	if err != nil {
		t.Fatalf("schedule message is not a valid event envelope: %v", err)
	}
	if kind != project.EventKind {
		t.Errorf("kind = %q", kind)
	}
	p, err := project.DecodeEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event != project.EventResearchDue || p.Slug != "housing-search" || p.ChatID != 42 {
		t.Errorf("payload = %+v", p)
	}

	// The dedup key is stamped back onto the project row.
	proj, err := st.GetProjectBySlug("housing-search")
	if err != nil || proj == nil {
		t.Fatalf("project read back: %v", err)
	}
	if proj.ScheduleDedupKey != "project:housing-search" {
		t.Errorf("schedule_dedup_key = %q", proj.ScheduleDedupKey)
	}
}

func TestProjectCreateRejectsUnknownCadence(t *testing.T) {
	s, _ := newScheduledProjectServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "X", "chat_id": 42, "cadence": "hourly",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("create returned %d: %v, want 400", code, out)
	}
}

func TestProjectStatusTogglesResearchSchedule(t *testing.T) {
	s, st := newScheduledProjectServer(t)

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "Demo", "chat_id": 42,
		"doc_path": "workspace/external.md", "cadence": "daily",
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}

	// Archive → disabled with a machine-readable reason.
	code, out = postProject(t, s, map[string]any{"action": "status", "slug": "demo", "status": "archived"})
	if code != http.StatusOK {
		t.Fatalf("status returned %d: %v", code, out)
	}
	sc, err := st.FindScheduleByDedupKey("project:demo")
	if err != nil || sc == nil {
		t.Fatalf("schedule lookup: %v", err)
	}
	if sc.Enabled || sc.PausedReason != "project_archived" {
		t.Errorf("after archive: enabled=%t reason=%q, want disabled/project_archived", sc.Enabled, sc.PausedReason)
	}

	// Re-activate → re-enabled with next run recomputed from now.
	code, out = postProject(t, s, map[string]any{"action": "status", "slug": "demo", "status": "active"})
	if code != http.StatusOK {
		t.Fatalf("status returned %d: %v", code, out)
	}
	sc, err = st.FindScheduleByDedupKey("project:demo")
	if err != nil || sc == nil {
		t.Fatalf("schedule lookup: %v", err)
	}
	if !sc.Enabled || sc.PausedReason != "" {
		t.Errorf("after re-activate: enabled=%t reason=%q, want enabled with reason cleared", sc.Enabled, sc.PausedReason)
	}
	if !sc.NextRunAt.After(time.Now().UTC().Add(-time.Minute)) {
		t.Errorf("next_run_at = %v, want recomputed into the future", sc.NextRunAt)
	}
}

func TestProjectMutationsRefreshHome(t *testing.T) {
	s, _ := newScheduledProjectServer(t)
	var refreshed []int64
	s.projectHomeRefresh = func(chatID int64) { refreshed = append(refreshed, chatID) }

	code, out := postProject(t, s, map[string]any{
		"action": "create", "title": "Demo", "chat_id": 42, "doc_path": "workspace/external.md",
	})
	if code != http.StatusOK {
		t.Fatalf("create returned %d: %v", code, out)
	}
	code, out = postProject(t, s, map[string]any{"action": "status", "slug": "demo", "status": "paused"})
	if code != http.StatusOK {
		t.Fatalf("status returned %d: %v", code, out)
	}

	if len(refreshed) != 2 || refreshed[0] != 42 || refreshed[1] != 42 {
		t.Errorf("home refreshes = %v, want create + status for chat 42", refreshed)
	}
}

func TestResearchCronStaggersOffTheHour(t *testing.T) {
	cases := []struct {
		cadence string
		id      int64
		want    string
	}{
		{"weekly", 3, "40 9 * * 1"},
		{"weekly", 5, "10 9 * * 1"},
		{"daily", 4, "50 9 * * *"},
		{"monthly", 10, "10 9 1 * *"},
		{"hourly", 1, ""},
	}
	for _, c := range cases {
		if got := researchCron(c.cadence, c.id); got != c.want {
			t.Errorf("researchCron(%q, %d) = %q, want %q", c.cadence, c.id, got, c.want)
		}
	}
	for id := int64(0); id < 25; id++ {
		if strings.HasPrefix(researchCron("weekly", id), "0 ") {
			t.Errorf("project %d landed on :00", id)
		}
	}
}
