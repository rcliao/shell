package topic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memory "github.com/rcliao/ghost"
)

const RegistryNS = "loop:topics"

// Registry persists per-chat topic taxonomy in ghost.
// Each topic is one ghost memory in ns "loop:topics" with key shape
// "topic-<chat_id>-<name>".
type Registry struct {
	store  memory.Store
	chatID int64
}

func NewRegistry(store memory.Store, chatID int64) *Registry {
	return &Registry{store: store, chatID: chatID}
}

func (r *Registry) key(topicName string) string {
	return fmt.Sprintf("topic-%d-%s", r.chatID, topicName)
}

// Get returns one topic by name, or nil if not found.
func (r *Registry) Get(ctx context.Context, name string) (*Topic, error) {
	mems, err := r.store.Get(ctx, memory.GetParams{
		NS:  RegistryNS,
		Key: r.key(name),
	})
	if err != nil {
		return nil, err
	}
	if len(mems) == 0 {
		return nil, nil
	}
	return parseTopic(mems[0].Content)
}

// List returns all topics for this chat (active only).
func (r *Registry) List(ctx context.Context) ([]Topic, error) {
	mems, err := r.store.List(ctx, memory.ListParams{
		NS:    RegistryNS,
		Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("topic-%d-", r.chatID)
	var out []Topic
	for _, m := range mems {
		if !strings.HasPrefix(m.Key, prefix) {
			continue
		}
		t, err := parseTopic(m.Content)
		if err != nil || t == nil || t.Status == "pruned" {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

// Upsert creates or updates a topic. Last-used and turn-count auto-bump.
func (r *Registry) Upsert(ctx context.Context, t Topic) error {
	if t.Name == "" {
		return fmt.Errorf("topic name required")
	}
	t.ChatID = r.chatID
	now := time.Now()
	t.LastUsed = now
	if t.FirstSeen.IsZero() {
		t.FirstSeen = now
	}
	if t.Status == "" {
		t.Status = "active"
	}
	buf, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = r.store.Put(ctx, memory.PutParams{
		NS:      RegistryNS,
		Key:     r.key(t.Name),
		Content: string(buf),
		Kind:    "semantic",
		Tier:    "ltm",
		Tags:    []string{"topic-registry", fmt.Sprintf("chat:%d", r.chatID), t.Name},
	})
	return err
}

// IncrementTurnCount is the common case after a turn classifies into an
// existing topic — updates last_used and turn_count without rewriting
// description/signal_examples.
func (r *Registry) IncrementTurnCount(ctx context.Context, name string) error {
	existing, err := r.Get(ctx, name)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("topic %q not in registry", name)
	}
	existing.TurnCount++
	existing.LastUsed = time.Now()
	return r.Upsert(ctx, *existing)
}

// Prune marks topics not used within `inactive` window as pruned (soft).
// Returns count pruned. Real garbage collection is ghost.GC's job.
func (r *Registry) Prune(ctx context.Context, inactive time.Duration) (int, error) {
	topics, err := r.List(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-inactive)
	count := 0
	for _, t := range topics {
		if t.LastUsed.Before(cutoff) {
			t.Status = "pruned"
			if err := r.Upsert(ctx, t); err == nil {
				count++
			}
		}
	}
	return count, nil
}

func parseTopic(content string) (*Topic, error) {
	var t Topic
	if err := json.Unmarshal([]byte(content), &t); err != nil {
		return nil, err
	}
	return &t, nil
}
