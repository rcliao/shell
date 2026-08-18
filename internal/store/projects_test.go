package store

import (
	"testing"
	"time"
)

func TestCreateProjectDerivesSlugAndDefaults(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	p, err := s.CreateProject(Project{Title: "Housing Search 2026", ChatID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if p.Slug != "housing-search-2026" {
		t.Errorf("slug = %q, want housing-search-2026", p.Slug)
	}
	if p.Status != "active" {
		t.Errorf("status = %q, want active", p.Status)
	}
	if p.NotifyPolicy != "quiet" {
		t.Errorf("notify_policy = %q, want quiet", p.NotifyPolicy)
	}
	if p.GhostTag != "project:housing-search-2026" {
		t.Errorf("ghost_tag = %q, want project:housing-search-2026", p.GhostTag)
	}
	if p.BlockMap != "{}" {
		t.Errorf("block_map = %q, want {}", p.BlockMap)
	}
	if p.HandledDiscussions != "[]" {
		t.Errorf("handled_discussions = %q, want []", p.HandledDiscussions)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Error("created_at/updated_at should be stamped")
	}
}

func TestCreateProjectSlugUniquenessSuffix(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	first, err := s.CreateProject(Project{Title: "Trip Plan", ChatID: 42})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateProject(Project{Title: "Trip Plan", ChatID: 42})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateProject(Project{Title: "Trip Plan", ChatID: -100200300})
	if err != nil {
		t.Fatal(err)
	}
	if first.Slug != "trip-plan" || second.Slug != "trip-plan-2" || third.Slug != "trip-plan-3" {
		t.Errorf("slugs = %q, %q, %q; want trip-plan, trip-plan-2, trip-plan-3",
			first.Slug, second.Slug, third.Slug)
	}
	if third.GhostTag != "project:trip-plan-3" {
		t.Errorf("suffixed slug must flow into ghost_tag, got %q", third.GhostTag)
	}
}

func TestSlugifyTitleCJKSafe(t *testing.T) {
	cases := map[string]string{
		"Housing Search 2026": "housing-search-2026",
		"日本 行程":               "日本-行程",
		"Trip: Tokyo & Kyoto!": "trip-tokyo-kyoto",
		"  spaced  out  ":      "spaced-out",
		"日本旅行":                 "日本旅行",
	}
	for in, want := range cases {
		if got := SlugifyTitle(in); got != want {
			t.Errorf("SlugifyTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateProjectRequiresTitle(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	if _, err := s.CreateProject(Project{ChatID: 42}); err == nil {
		t.Error("expected error for missing title")
	}
	if _, err := s.CreateProject(Project{Title: "!!!", ChatID: 42}); err == nil {
		t.Error("expected error for a title that slugifies to nothing")
	}
	if _, err := s.CreateProject(Project{Title: "x", ChatID: 42, Status: "bogus"}); err == nil {
		t.Error("expected error for invalid status")
	}
}

func TestGetProjectBySlugAbsent(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	p, err := s.GetProjectBySlug("no-such-project")
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Errorf("expected nil for absent slug, got %+v", p)
	}
}

func TestListProjectsByChat(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	if _, err := s.CreateProject(Project{Title: "Alpha", ChatID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(Project{Title: "Beta", ChatID: 42}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(Project{Title: "Gamma", ChatID: -100200300}); err != nil {
		t.Fatal(err)
	}

	forChat, err := s.ListProjects(42)
	if err != nil {
		t.Fatal(err)
	}
	if len(forChat) != 2 {
		t.Errorf("chat 42 should have 2 projects, got %d", len(forChat))
	}

	all, err := s.ListProjects(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("chatID=0 should list all 3 projects, got %d", len(all))
	}
}

func TestUpdateProjectStatus(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	if _, err := s.CreateProject(Project{Title: "Lifecycle", ChatID: 42}); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateProjectStatus("lifecycle", "archived"); err != nil {
		t.Fatal(err)
	}
	p, _ := s.GetProjectBySlug("lifecycle")
	if p.Status != "archived" {
		t.Errorf("status = %q, want archived", p.Status)
	}

	if err := s.UpdateProjectStatus("lifecycle", "sideways"); err == nil {
		t.Error("expected error for invalid status")
	}
	if err := s.UpdateProjectStatus("no-such", "paused"); err == nil {
		t.Error("expected error for unknown slug")
	}
}

func TestUpdateProjectFields(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	if _, err := s.CreateProject(Project{Title: "Fields", ChatID: 42}); err != nil {
		t.Fatal(err)
	}

	ref := "notion-page-abc123"
	kind := "notion"
	docPath := "workspace/projects/fields/doc.md"
	research := time.Now().UTC().Truncate(time.Second)
	err := s.UpdateProjectFields("fields", ProjectFieldUpdate{
		ExportKind:     &kind,
		ExportRef:      &ref,
		DocPath:        &docPath,
		LastResearchAt: &research,
	})
	if err != nil {
		t.Fatal(err)
	}

	p, _ := s.GetProjectBySlug("fields")
	if p.ExportRef != ref || p.ExportKind != kind || p.DocPath != docPath {
		t.Errorf("fields not applied: %+v", p)
	}
	if p.LastResearchAt == nil || !p.LastResearchAt.Equal(research) {
		t.Errorf("last_research_at = %v, want %v", p.LastResearchAt, research)
	}
	// Untouched fields survive.
	if p.Status != "active" || p.GhostTag != "project:fields" {
		t.Errorf("untouched fields changed: %+v", p)
	}

	if err := s.UpdateProjectFields("fields", ProjectFieldUpdate{}); err == nil {
		t.Error("expected error when no fields are set")
	}
	if err := s.UpdateProjectFields("no-such", ProjectFieldUpdate{ExportRef: &ref}); err == nil {
		t.Error("expected error for unknown slug")
	}
}
