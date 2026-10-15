package memory

import (
	"context"
	"testing"
)

func TestGoalLessonsStoreAndRecall(t *testing.T) {
	m := newTestMemory(t)
	ctx := context.Background()

	keys, err := m.StoreGoalLessons(ctx, 42, "project:garden", 7, []string{"- check the maker's site first", "", "prices drop on Fridays", "two shops were out of stock", "a fourth is dropped"})
	if err != nil || len(keys) != GoalLessonMax {
		t.Fatalf("store: %v %v, want %d keys", keys, err, GoalLessonMax)
	}
	// A retried close sends the same lesson again: it is not written twice.
	if again, err := m.StoreGoalLessons(ctx, 42, "project:garden", 7, []string{"check the maker's site first"}); err != nil || len(again) != 0 {
		t.Errorf("repeat lesson: %v %v, want none written", again, err)
	}
	m.StoreGoalLessons(ctx, 42, "project:kitchen", 8, []string{"measure the counter first"})

	got, err := m.GoalLessons(ctx, 42, "project:garden", 5)
	if err != nil || len(got) != 3 {
		t.Fatalf("recall: %+v %v, want 3", got, err)
	}
	seen := map[string]bool{}
	for _, l := range got {
		seen[l.Content] = true
	}
	for _, want := range []string{"check the maker's site first", "prices drop on Fridays", "two shops were out of stock"} {
		if !seen[want] {
			t.Errorf("recall is missing %q: %+v", want, got)
		}
	}
	if seen["measure the counter first"] {
		t.Error("another project's lesson was recalled")
	}
	if _, err := m.StoreGoalLessons(ctx, 999, "project:garden", 7, []string{"x"}); err == nil {
		t.Error("a chat with no agent namespace stored a lesson")
	}
}
