// Package memory wraps the agent-memory library for use in teeny-relay.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	agentmemory "github.com/rcliao/agent-memory"
)

// Memory wraps an agent-memory store for relay use.
type Memory struct {
	store            agentmemory.Store
	budget           int
	globalNamespaces []string
	globalBudget     int
	systemNamespaces []string
	systemBudget     int
}

// New opens or creates a memory store at the given path.
func New(dbPath string, budget int, globalNamespaces []string, globalBudget int, systemNamespaces []string, systemBudget int) (*Memory, error) {
	s, err := agentmemory.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open memory store: %w", err)
	}
	if budget <= 0 {
		budget = 2000
	}
	if globalBudget <= 0 {
		globalBudget = 500
	}
	if systemBudget <= 0 {
		systemBudget = 3000
	}
	return &Memory{
		store:            s,
		budget:           budget,
		globalNamespaces: globalNamespaces,
		globalBudget:     globalBudget,
		systemNamespaces: systemNamespaces,
		systemBudget:     systemBudget,
	}, nil
}

// SystemPrompt returns always-on context loaded via List() (no search).
// Returns empty string if no system namespaces are configured or no memories found.
func (m *Memory) SystemPrompt(ctx context.Context) string {
	if len(m.systemNamespaces) == 0 {
		return ""
	}

	charBudget := m.systemBudget * 4
	var sb strings.Builder
	used := 0

	for _, ns := range m.systemNamespaces {
		// Derive a section heading from the last segment of the namespace.
		// e.g. "openclaw:identity" → "Identity"
		heading := ns
		if idx := strings.LastIndex(ns, ":"); idx >= 0 && idx+1 < len(ns) {
			heading = ns[idx+1:]
		}
		heading = strings.ToUpper(heading[:1]) + heading[1:]

		memories, err := m.store.List(ctx, agentmemory.ListParams{
			NS:    ns,
			Limit: 100,
		})
		if err != nil {
			slog.Warn("system prompt list failed", "ns", ns, "error", err)
			continue
		}
		if len(memories) == 0 {
			continue
		}

		section := fmt.Sprintf("## %s\n", heading)
		for _, mem := range memories {
			section += fmt.Sprintf("- %s\n", mem.Content)
		}
		section += "\n"

		if used+len(section) > charBudget {
			break
		}
		sb.WriteString(section)
		used += len(section)
	}

	return strings.TrimSpace(sb.String())
}

// namespace returns the per-chat namespace.
func namespace(chatID int64) string {
	return fmt.Sprintf("relay:chat:%d", chatID)
}

// InjectContext fetches relevant memories and prepends them to the user message.
// It queries two layers: global background context and per-chat conversation memories.
func (m *Memory) InjectContext(ctx context.Context, chatID int64, userMsg string) string {
	var sb strings.Builder

	// Layer 1: global background context
	if len(m.globalNamespaces) > 0 {
		globalMems := m.fetchGlobalContext(ctx, userMsg)
		if len(globalMems) > 0 {
			sb.WriteString("[Background context]\n")
			for _, mem := range globalMems {
				sb.WriteString("- ")
				sb.WriteString(mem.Content)
				sb.WriteString("\n")
			}
			sb.WriteString("[End of background context]\n\n")
		}
	}

	// Layer 2: per-chat conversation memories
	ns := namespace(chatID)
	result, err := m.store.Context(ctx, agentmemory.ContextParams{
		NS:     ns + "*",
		Query:  userMsg,
		Budget: m.budget,
	})
	if err != nil {
		slog.Warn("memory context fetch failed", "error", err)
	} else if len(result.Memories) > 0 {
		sb.WriteString("[Relevant memories from previous conversations]\n")
		for _, mem := range result.Memories {
			sb.WriteString("- ")
			sb.WriteString(mem.Content)
			sb.WriteString("\n")
		}
		sb.WriteString("[End of memories]\n\n")
	}

	if sb.Len() == 0 {
		return userMsg
	}

	sb.WriteString(userMsg)
	return sb.String()
}

// fetchGlobalContext queries each global namespace pattern, merges results by score,
// and trims to the global character budget.
func (m *Memory) fetchGlobalContext(ctx context.Context, query string) []agentmemory.ContextMemory {
	charBudget := m.globalBudget * 4

	var all []agentmemory.ContextMemory
	for _, ns := range m.globalNamespaces {
		pattern := ns
		if !strings.HasSuffix(pattern, "*") {
			pattern += "*"
		}
		result, err := m.store.Context(ctx, agentmemory.ContextParams{
			NS:     pattern,
			Query:  query,
			Budget: m.globalBudget,
		})
		if err != nil {
			slog.Warn("global context fetch failed", "ns", ns, "error", err)
			continue
		}
		all = append(all, result.Memories...)
	}

	// Sort by score descending
	sort.Slice(all, func(i, j int) bool {
		return all[i].Score > all[j].Score
	})

	// Trim to char budget
	var result []agentmemory.ContextMemory
	used := 0
	for _, mem := range all {
		if used+len(mem.Content) > charBudget {
			break
		}
		result = append(result, mem)
		used += len(mem.Content)
	}
	return result
}

// LogExchange stores a summary of the user/assistant exchange as episodic memory.
func (m *Memory) LogExchange(ctx context.Context, chatID int64, userMsg, response string) {
	ns := namespace(chatID)

	// Truncate for storage — keep a reasonable summary
	summary := userMsg
	if len(summary) > 200 {
		summary = summary[:200] + "..."
	}
	respSummary := response
	if len(respSummary) > 300 {
		respSummary = respSummary[:300] + "..."
	}

	content := fmt.Sprintf("User: %s\nAssistant: %s", summary, respSummary)

	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:      ns,
		Key:     fmt.Sprintf("exchange-%d", time.Now().UnixMilli()),
		Content: content,
		Kind:    "episodic",
		TTL:     "7d",
	})
	if err != nil {
		slog.Warn("failed to log exchange to memory", "error", err)
	}
}

// Remember stores a user-provided memory as semantic memory.
func (m *Memory) Remember(ctx context.Context, chatID int64, content string) error {
	ns := namespace(chatID)
	// Use a key derived from content prefix to allow multiple memories
	key := sanitizeKey(content)
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:       ns,
		Key:      key,
		Content:  content,
		Kind:     "semantic",
		Priority: "high",
	})
	return err
}

// Forget removes a memory by key.
func (m *Memory) Forget(ctx context.Context, chatID int64, key string) error {
	ns := namespace(chatID)
	return m.store.Rm(ctx, agentmemory.RmParams{
		NS:  ns,
		Key: key,
	})
}

// ListMemories returns a formatted list of memories for a chat.
func (m *Memory) ListMemories(ctx context.Context, chatID int64) (string, error) {
	ns := namespace(chatID)
	memories, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "semantic",
		Limit: 20,
	})
	if err != nil {
		return "", err
	}

	if len(memories) == 0 {
		return "No memories stored. Use /remember <text> to save one.", nil
	}

	var sb strings.Builder
	sb.WriteString("Stored memories:\n\n")
	for i, mem := range memories {
		sb.WriteString(fmt.Sprintf("%d. [%s] %s\n", i+1, mem.Key, mem.Content))
	}
	sb.WriteString(fmt.Sprintf("\nTotal: %d memories", len(memories)))
	return sb.String(), nil
}

// Close closes the underlying store.
func (m *Memory) Close() error {
	return m.store.Close()
}

// sanitizeKey creates a short key from content text.
func sanitizeKey(content string) string {
	// Take first ~40 chars, lowercase, replace spaces with dashes
	key := strings.ToLower(content)
	if len(key) > 40 {
		key = key[:40]
	}
	key = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		if r == ' ' {
			return '-'
		}
		return -1
	}, key)
	// Trim trailing dashes
	key = strings.Trim(key, "-")
	if key == "" {
		key = "memory"
	}
	return key
}
