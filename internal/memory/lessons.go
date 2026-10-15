package memory

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentmemory "github.com/rcliao/ghost"
)

// Goal lessons (docs/DESIGN-AGENT-LOOPS.md step 4, LessonStore). When a goal
// schedule closes, the agent leaves up to GoalLessonMax lessons; every later
// goal fire in the same project gets the newest ones in its prompt. They are
// fetched by tag, not by query: per-turn recall is keyed on the latest
// message and would not find them.

// GoalLessonMax is how many lessons one goal close may write.
const GoalLessonMax = 3

// GoalLessonTag marks a goal lesson in ghost.
const GoalLessonTag = "lesson"

// GoalLesson is one stored lesson.
type GoalLesson struct {
	Key     string
	Content string
}

// StoreGoalLessons writes a closed goal's lessons into the agent namespace
// of the project's chat, tagged lesson + the project's ghost tag +
// goal:<id>. Empty lines are skipped and at most GoalLessonMax are kept.
// It returns the keys written; a lesson the project already has is skipped.
func (m *Memory) StoreGoalLessons(ctx context.Context, chatID int64, projectTag string, scheduleID int64, lessons []string) ([]string, error) {
	ns := m.profileFor(chatID).AgentNS
	if ns == "" {
		return nil, fmt.Errorf("no agent namespace for chat %d", chatID)
	}
	if projectTag == "" {
		return nil, fmt.Errorf("goal lessons need a project tag")
	}
	// A retried last run may send the same lessons again: skip any this
	// project already has. Ghost's own Dedup is not used — it would fold a
	// lesson into a look-alike memory that lacks the lesson tags.
	have := map[string]bool{}
	if old, err := m.GoalLessons(ctx, chatID, projectTag, 50); err == nil {
		for _, o := range old {
			have[o.Content] = true
		}
	}
	var keys []string
	stamp := time.Now().UnixMilli()
	for _, l := range lessons {
		l = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(l), "-•*"))
		if l == "" || have[l] {
			continue
		}
		have[l] = true
		if len(keys) == GoalLessonMax {
			break
		}
		key := fmt.Sprintf("lesson-goal-%d-%d-%d", scheduleID, stamp, len(keys)+1)
		if _, err := m.store.Put(ctx, agentmemory.PutParams{
			NS:         ns,
			Key:        key,
			Content:    l,
			Kind:       "procedural",
			Tags:       []string{GoalLessonTag, projectTag, fmt.Sprintf("goal:%d", scheduleID)},
			Importance: 0.7,
			SourceKind: "self",
		}); err != nil {
			return keys, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// GoalLessons returns the newest goal lessons for a project, at most limit.
func (m *Memory) GoalLessons(ctx context.Context, chatID int64, projectTag string, limit int) ([]GoalLesson, error) {
	ns := m.profileFor(chatID).AgentNS
	if ns == "" || projectTag == "" {
		return nil, nil
	}
	mems, err := m.store.List(ctx, agentmemory.ListParams{NS: ns, Tags: []string{GoalLessonTag, projectTag}, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]GoalLesson, 0, len(mems))
	for _, mem := range mems {
		out = append(out, GoalLesson{Key: mem.Key, Content: mem.Content})
	}
	return out, nil
}
