package main

import (
	"testing"

	"github.com/rcliao/shell/internal/store"
)

func TestScoreRoutesAgainstBaseline(t *testing.T) {
	// Five messages in one thread; labels: g, g, japan, japan, g.
	labels := map[string]store.RouteLabel{}
	lanes := []string{"general", "general", "japan", "japan", "general"}
	for i, l := range lanes {
		h := string(rune('a' + i))
		labels[store.LabelKey(42, 0, h)] = store.RouteLabel{Lane: l, Source: "judge"}
	}
	// jev gets 4/5: it finds one japan message and holds it (sticky) one too long.
	jev := []string{"general", "general", "japan", "japan", "japan"}
	var rows []store.RouteDecision
	for i, l := range jev {
		rows = append(rows, store.RouteDecision{Backend: "jev", ChatID: 42, TextHash: string(rune('a' + i)), Lane: l, Sticky: i == 4})
	}
	s, src := scoreRoutes(rows, labels)
	j := s["jev"]
	if j.labelled != 5 || j.correct != 4 {
		t.Errorf("agreement = %d/%d, want 4/5", j.correct, j.labelled)
	}
	if j.nonGeneral != 2 || j.nonGeneralHit != 2 {
		t.Errorf("project messages found = %d/%d, want 2/2", j.nonGeneralHit, j.nonGeneral)
	}
	if j.predProject != 3 || j.predProjectHit != 2 {
		t.Errorf("project picks right = %d/%d, want 2/3", j.predProjectHit, j.predProject)
	}
	if j.generalLabels != 3 {
		t.Errorf("always-general baseline would agree on %d, want 3", j.generalLabels)
	}
	if j.transitions != 4 || j.changes != 1 || j.stickyRows != 1 {
		t.Errorf("churn: %d changes / %d transitions, sticky %d", j.changes, j.transitions, j.stickyRows)
	}
	if src["judge"] != 5 {
		t.Errorf("label sources = %v", src)
	}
}
