// Package memory wraps the agent-memory library for use in teeny-relay.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	agentmemory "github.com/rcliao/agent-memory"
)

// ProfileConfig configures memory behavior for a specific agent profile.
type ProfileConfig struct {
	SystemNamespaces []string
	SystemBudget     int
	GlobalNamespaces []string
	GlobalBudget     int
	Budget           int
	ExchangeTTL      string // "7d", "30d"
	ExchangeMaxUser  int    // 0 = default 200
	ExchangeMaxReply int    // 0 = default 300
	MemoryDirectives bool
	DirectiveNS      string // target NS for [remember] blocks
}

// Memory wraps an agent-memory store for relay use.
type Memory struct {
	store            agentmemory.Store
	budget           int
	globalNamespaces []string
	globalBudget     int
	systemNamespaces []string
	systemBudget     int
	profiles         map[string]ProfileConfig
	chatProfiles     map[int64]string
}

// New opens or creates a memory store at the given path.
func New(dbPath string, budget int, globalNamespaces []string, globalBudget int, systemNamespaces []string, systemBudget int, profiles map[string]ProfileConfig, chatProfiles map[int64]string) (*Memory, error) {
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
		profiles:         profiles,
		chatProfiles:     chatProfiles,
	}, nil
}

// profileFor resolves the profile for a chat, falling back to top-level defaults.
func (m *Memory) profileFor(chatID int64) ProfileConfig {
	if name, ok := m.chatProfiles[chatID]; ok {
		if p, ok := m.profiles[name]; ok {
			return p
		}
	}
	// Default profile from top-level config values
	return ProfileConfig{
		SystemNamespaces: m.systemNamespaces,
		SystemBudget:     m.systemBudget,
		GlobalNamespaces: m.globalNamespaces,
		GlobalBudget:     m.globalBudget,
		Budget:           m.budget,
		ExchangeTTL:      "7d",
		ExchangeMaxUser:  200,
		ExchangeMaxReply: 300,
	}
}

// SystemPrompt returns always-on context loaded via List() (no search).
// Uses the profile for the given chatID to determine namespaces and budget.
func (m *Memory) SystemPrompt(ctx context.Context, chatID int64) string {
	prof := m.profileFor(chatID)
	if len(prof.SystemNamespaces) == 0 {
		return ""
	}

	charBudget := prof.SystemBudget * 4
	var sb strings.Builder
	used := 0

	for _, ns := range prof.SystemNamespaces {
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
	prof := m.profileFor(chatID)
	var sb strings.Builder

	// Layer 1: global background context
	if len(prof.GlobalNamespaces) > 0 && prof.GlobalBudget > 0 {
		globalMems := m.fetchGlobalContextFor(ctx, userMsg, prof.GlobalNamespaces, prof.GlobalBudget)
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
		Budget: prof.Budget,
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

// fetchGlobalContextFor queries each global namespace pattern, merges results by score,
// and trims to the global character budget.
func (m *Memory) fetchGlobalContextFor(ctx context.Context, query string, namespaces []string, budget int) []agentmemory.ContextMemory {
	charBudget := budget * 4

	var all []agentmemory.ContextMemory
	for _, ns := range namespaces {
		pattern := ns
		if !strings.HasSuffix(pattern, "*") {
			pattern += "*"
		}
		result, err := m.store.Context(ctx, agentmemory.ContextParams{
			NS:     pattern,
			Query:  query,
			Budget: budget,
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
	prof := m.profileFor(chatID)
	ns := namespace(chatID)

	maxUser := prof.ExchangeMaxUser
	if maxUser <= 0 {
		maxUser = 200
	}
	maxReply := prof.ExchangeMaxReply
	if maxReply <= 0 {
		maxReply = 300
	}

	summary := userMsg
	if len(summary) > maxUser {
		summary = summary[:maxUser] + "..."
	}
	respSummary := response
	if len(respSummary) > maxReply {
		respSummary = respSummary[:maxReply] + "..."
	}

	ttl := prof.ExchangeTTL
	if ttl == "" {
		ttl = "7d"
	}

	content := fmt.Sprintf("User: %s\nAssistant: %s", summary, respSummary)

	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:      ns,
		Key:     fmt.Sprintf("exchange-%d", time.Now().UnixMilli()),
		Content: content,
		Kind:    "episodic",
		TTL:     ttl,
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
	sb.WriteString("## Memories\n\n")
	for i, mem := range memories {
		sb.WriteString(fmt.Sprintf("%d. **%s** — %s\n", i+1, mem.Key, mem.Content))
	}
	sb.WriteString(fmt.Sprintf("\n---\n\n*%d memories stored*", len(memories)))
	return sb.String(), nil
}

// directiveRe matches [remember kind=procedural]...[/remember] blocks.
var directiveRe = regexp.MustCompile(`(?s)\[remember(?:\s+kind=(\w+))?\]\s*(.*?)\s*\[/remember\]`)

// ParseMemoryDirectives extracts [remember]...[/remember] blocks from the response,
// stores them to the profile's directive namespace, and returns the cleaned response.
// Only active when the chat's profile has MemoryDirectives enabled.
func (m *Memory) ParseMemoryDirectives(ctx context.Context, chatID int64, response string) string {
	prof := m.profileFor(chatID)
	if !prof.MemoryDirectives || prof.DirectiveNS == "" {
		return response
	}

	matches := directiveRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response
	}

	clean := response
	for i := len(matches) - 1; i >= 0; i-- {
		loc := matches[i]
		kind := "semantic"
		if loc[2] >= 0 && loc[3] >= 0 {
			kind = response[loc[2]:loc[3]]
		}
		content := strings.TrimSpace(response[loc[4]:loc[5]])
		clean = clean[:loc[0]] + clean[loc[1]:]

		// Store the directive
		_, err := m.store.Put(ctx, agentmemory.PutParams{
			NS:      prof.DirectiveNS,
			Key:     fmt.Sprintf("learning-%d", time.Now().UnixMilli()),
			Content: content,
			Kind:    kind,
		})
		if err != nil {
			slog.Warn("failed to store memory directive", "ns", prof.DirectiveNS, "error", err)
		} else {
			slog.Info("stored memory directive", "ns", prof.DirectiveNS, "kind", kind, "len", len(content))
		}
	}

	return strings.TrimSpace(clean)
}

// ReviewerContext queries the store for relevant reviewer learnings in the given
// namespace and returns a formatted bullet list. Returns "" if nothing relevant is found.
func (m *Memory) ReviewerContext(ctx context.Context, namespace, query string, budget int) string {
	if budget <= 0 {
		budget = 500
	}
	result, err := m.store.Context(ctx, agentmemory.ContextParams{
		NS:     namespace + "*",
		Query:  query,
		Budget: budget,
	})
	if err != nil {
		slog.Warn("reviewer context fetch failed", "ns", namespace, "error", err)
		return ""
	}
	if len(result.Memories) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, mem := range result.Memories {
		sb.WriteString("- ")
		sb.WriteString(mem.Content)
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// StoreReviewerLearning stores a reviewer learning as semantic/high-priority memory.
// No TTL — critical flow knowledge shouldn't expire.
func (m *Memory) StoreReviewerLearning(ctx context.Context, namespace, content string) error {
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:       namespace,
		Key:      fmt.Sprintf("reviewer-%d", time.Now().UnixMilli()),
		Content:  content,
		Kind:     "semantic",
		Priority: "high",
	})
	return err
}

// ReviewEntry holds a memory reference for the review index.
type ReviewEntry struct {
	NS  string
	Key string
}

// ReviewMemories returns a formatted summary of all memories (semantic + episodic)
// and a lookup slice for correction by index.
func (m *Memory) ReviewMemories(ctx context.Context, chatID int64) (string, []ReviewEntry, error) {
	ns := namespace(chatID)

	// Fetch semantic memories
	semantic, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "semantic",
		Limit: 50,
	})
	if err != nil {
		return "", nil, err
	}

	// Fetch episodic memories
	episodic, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "episodic",
		Limit: 20,
	})
	if err != nil {
		return "", nil, err
	}

	if len(semantic) == 0 && len(episodic) == 0 {
		return "No memories found. Use /remember <text> to save one.", nil, nil
	}

	var sb strings.Builder
	var entries []ReviewEntry
	idx := 1

	if len(semantic) > 0 {
		sb.WriteString("## Saved Memories\n\n")
		for _, mem := range semantic {
			sb.WriteString(fmt.Sprintf("`%d.` **%s**\n%s\n\n", idx, mem.Key, mem.Content))
			entries = append(entries, ReviewEntry{NS: ns, Key: mem.Key})
			idx++
		}
	}

	if len(episodic) > 0 {
		sb.WriteString("## Recent Conversations\n\n")
		for _, mem := range episodic {
			// Truncate long episodic entries for summary
			content := mem.Content
			if len(content) > 120 {
				content = content[:120] + "..."
			}
			sb.WriteString(fmt.Sprintf("`%d.` %s\n\n", idx, content))
			entries = append(entries, ReviewEntry{NS: ns, Key: mem.Key})
			idx++
		}
	}

	sb.WriteString(fmt.Sprintf("---\n\n*%d total memories* — Use `/correct <number> <new text>` to fix or `/forget <key>` to remove", len(entries)))
	return sb.String(), entries, nil
}

// CorrectMemory updates the content of a memory identified by namespace and key.
func (m *Memory) CorrectMemory(ctx context.Context, ns, key, newContent string) error {
	// Look up the existing memory to preserve its kind/priority
	existing, err := m.store.Get(ctx, agentmemory.GetParams{
		NS:  ns,
		Key: key,
	})
	if err != nil {
		return fmt.Errorf("get memory: %w", err)
	}

	kind := "semantic"
	priority := "high"
	if len(existing) > 0 {
		kind = existing[0].Kind
		if existing[0].Priority != "" {
			priority = existing[0].Priority
		}
	}

	_, err = m.store.Put(ctx, agentmemory.PutParams{
		NS:       ns,
		Key:      key,
		Content:  newContent,
		Kind:     kind,
		Priority: priority,
	})
	return err
}

// SeedNamespace stores a high-priority semantic memory in the given namespace.
// Uses Put with the same NS+key, so repeated calls are idempotent (upserts).
func (m *Memory) SeedNamespace(ctx context.Context, ns, key, content string) error {
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:       ns,
		Key:      key,
		Content:  content,
		Kind:     "semantic",
		Priority: "high",
	})
	return err
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
