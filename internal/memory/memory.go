// Package memory wraps the ghost library for use in shell.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	agentmemory "github.com/rcliao/ghost"
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

// Memory wraps a ghost store for shell use.
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
	return fmt.Sprintf("shell:chat:%d", chatID)
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

	// Layer 2: per-chat conversation memories (with tier-aware pinning)
	ns := namespace(chatID)
	result, err := m.store.Context(ctx, agentmemory.ContextParams{
		NS:       ns + "*",
		Query:    userMsg,
		Budget:   prof.Budget,
		PinTiers: []string{"identity", "ltm"}, // always inject important long-term memories first
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
	if utf8.RuneCountInString(summary) > maxUser {
		summary = string([]rune(summary)[:maxUser]) + "..."
	}
	respSummary := response
	if utf8.RuneCountInString(respSummary) > maxReply {
		respSummary = string([]rune(respSummary)[:maxReply]) + "..."
	}

	ttl := prof.ExchangeTTL
	if ttl == "" {
		ttl = "7d"
	}

	content := fmt.Sprintf("User: %s\nAssistant: %s", summary, respSummary)

	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        fmt.Sprintf("exchange-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       "episodic",
		TTL:        ttl,
		Importance: 0.3, // ephemeral exchanges — low importance, will decay naturally
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
		NS:         ns,
		Key:        key,
		Content:    content,
		Kind:       "semantic",
		Priority:   "high",
		Importance: 0.8, // user-remembered facts are high importance
		Tier:       "ltm", // explicitly saved by user — skip stm
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

// ListMemories returns a formatted debug view of all memories across related namespaces.
func (m *Memory) ListMemories(ctx context.Context, chatID int64) (string, error) {
	prof := m.profileFor(chatID)
	chatNS := namespace(chatID)
	hbNS := heartbeatNamespace(chatID)

	// Collect all namespaces to query
	type nsGroup struct {
		label string
		ns    string
	}
	groups := []nsGroup{
		{"Chat (semantic)", chatNS},
		{"Chat (episodic)", chatNS},
		{"Heartbeat", hbNS},
	}

	// Add system namespaces from profile
	for _, ns := range prof.SystemNamespaces {
		groups = append(groups, nsGroup{"System: " + ns, ns})
	}
	// Add global namespaces from profile
	for _, ns := range prof.GlobalNamespaces {
		groups = append(groups, nsGroup{"Global: " + ns, ns})
	}
	// Add directive namespace if configured
	if prof.DirectiveNS != "" {
		groups = append(groups, nsGroup{"Directives: " + prof.DirectiveNS, prof.DirectiveNS})
	}

	var sb strings.Builder
	totalCount := 0

	// Peek for tier overview
	peek, err := m.store.Peek(ctx, chatNS+"*")
	if err == nil && len(peek.MemoryCounts) > 0 {
		sb.WriteString("## Memory Overview\n\n")
		for tier, count := range peek.MemoryCounts {
			tokens := peek.TotalEstTokens[tier]
			sb.WriteString(fmt.Sprintf("- **%s**: %d memories (~%d tokens)\n", tier, count, tokens))
		}
		if peek.IdentitySummary != "" {
			sb.WriteString(fmt.Sprintf("\nIdentity: %s\n", peek.IdentitySummary))
		}
		sb.WriteString("\n")
	}

	// Chat semantic memories
	semantic, _ := m.store.List(ctx, agentmemory.ListParams{NS: chatNS, Kind: "semantic", Limit: 50})
	if len(semantic) > 0 {
		sb.WriteString("## Saved Memories\n\n")
		for _, mem := range semantic {
			sb.WriteString(fmt.Sprintf("- **%s** [%s, imp=%.1f, acc=%d] — %s\n",
				mem.Key, mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 120)))
			totalCount++
		}
		sb.WriteString("\n")
	}

	// Chat episodic memories
	episodic, _ := m.store.List(ctx, agentmemory.ListParams{NS: chatNS, Kind: "episodic", Limit: 10})
	if len(episodic) > 0 {
		sb.WriteString("## Recent Exchanges\n\n")
		for _, mem := range episodic {
			sb.WriteString(fmt.Sprintf("- [%s, imp=%.1f, acc=%d] %s\n",
				mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 100)))
			totalCount++
		}
		sb.WriteString("\n")
	}

	// Heartbeat learnings
	hbMems, _ := m.store.List(ctx, agentmemory.ListParams{NS: hbNS, Kind: "semantic", Limit: 20})
	if len(hbMems) > 0 {
		sb.WriteString("## Heartbeat Learnings\n\n")
		for _, mem := range hbMems {
			sb.WriteString(fmt.Sprintf("- [%s, imp=%.1f, acc=%d] %s\n",
				mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 120)))
			totalCount++
		}
		sb.WriteString("\n")
	}

	// System namespace memories
	for _, ns := range prof.SystemNamespaces {
		sysMems, _ := m.store.List(ctx, agentmemory.ListParams{NS: ns, Limit: 20})
		if len(sysMems) > 0 {
			sb.WriteString(fmt.Sprintf("## System: %s\n\n", ns))
			for _, mem := range sysMems {
				sb.WriteString(fmt.Sprintf("- **%s** [%s, imp=%.1f] — %s\n",
					mem.Key, mem.Tier, mem.Importance, truncateStr(mem.Content, 80)))
				totalCount++
			}
			sb.WriteString("\n")
		}
	}

	// Global namespace memories
	for _, ns := range prof.GlobalNamespaces {
		globalMems, _ := m.store.List(ctx, agentmemory.ListParams{NS: ns, Limit: 20})
		if len(globalMems) > 0 {
			sb.WriteString(fmt.Sprintf("## Global: %s\n\n", ns))
			for _, mem := range globalMems {
				sb.WriteString(fmt.Sprintf("- **%s** [%s, imp=%.1f] — %s\n",
					mem.Key, mem.Tier, mem.Importance, truncateStr(mem.Content, 80)))
				totalCount++
			}
			sb.WriteString("\n")
		}
	}

	// Directive namespace memories
	if prof.DirectiveNS != "" {
		dirMems, _ := m.store.List(ctx, agentmemory.ListParams{NS: prof.DirectiveNS, Limit: 20})
		if len(dirMems) > 0 {
			sb.WriteString(fmt.Sprintf("## Directives: %s\n\n", prof.DirectiveNS))
			for _, mem := range dirMems {
				sb.WriteString(fmt.Sprintf("- **%s** [%s, imp=%.1f] — %s\n",
					mem.Key, mem.Tier, mem.Importance, truncateStr(mem.Content, 80)))
				totalCount++
			}
			sb.WriteString("\n")
		}
	}

	if totalCount == 0 {
		return "No memories found across any namespace.\nUse /remember <text> to save one.", nil
	}

	sb.WriteString(fmt.Sprintf("---\n\n*%d total memories across all namespaces*", totalCount))
	return sb.String(), nil
}

func truncateStr(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "..."
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
			NS:         prof.DirectiveNS,
			Key:        fmt.Sprintf("learning-%d", time.Now().UnixMilli()),
			Content:    content,
			Kind:       kind,
			Importance: 0.7, // agent-discovered learnings
		})
		if err != nil {
			slog.Warn("failed to store memory directive", "ns", prof.DirectiveNS, "error", err)
		} else {
			slog.Info("stored memory directive", "ns", prof.DirectiveNS, "kind", kind, "len", len(content))
		}
	}

	return strings.TrimSpace(clean)
}

// heartbeatNamespace returns the per-chat heartbeat learning namespace.
func heartbeatNamespace(chatID int64) string {
	return fmt.Sprintf("shell:heartbeat:%d", chatID)
}

// RecentExchanges returns the last N episodic exchanges for a chat.
func (m *Memory) RecentExchanges(ctx context.Context, chatID int64, limit int) []string {
	if limit <= 0 {
		limit = 10
	}
	memories, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    namespace(chatID),
		Kind:  "episodic",
		Limit: limit,
	})
	if err != nil {
		slog.Warn("recent exchanges fetch failed", "error", err)
		return nil
	}
	var result []string
	for _, mem := range memories {
		result = append(result, mem.Content)
	}
	return result
}

// maxHeartbeatLearnings is the cap on stored heartbeat learnings per chat.
const maxHeartbeatLearnings = 20

// StoreHeartbeatLearning stores a heartbeat learning as semantic/high-priority memory.
// Prunes oldest entries beyond maxHeartbeatLearnings.
func (m *Memory) StoreHeartbeatLearning(ctx context.Context, chatID int64, content string) error {
	ns := heartbeatNamespace(chatID)
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        fmt.Sprintf("hb-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       "semantic",
		Priority:   "high",
		Importance: 0.7, // heartbeat learnings — moderately important, self-discovered
	})
	if err != nil {
		return err
	}

	// Prune oldest learnings beyond cap
	all, listErr := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "semantic",
		Limit: maxHeartbeatLearnings + 20, // fetch extra to find ones to delete
	})
	if listErr != nil {
		slog.Warn("heartbeat learning prune list failed", "error", listErr)
		return nil
	}
	if len(all) > maxHeartbeatLearnings {
		// List returns newest first; delete from the end
		for _, mem := range all[maxHeartbeatLearnings:] {
			if rmErr := m.store.Rm(ctx, agentmemory.RmParams{NS: ns, Key: mem.Key}); rmErr != nil {
				slog.Warn("heartbeat learning prune failed", "key", mem.Key, "error", rmErr)
			}
		}
		slog.Info("pruned heartbeat learnings", "chat_id", chatID, "removed", len(all)-maxHeartbeatLearnings)
	}
	return nil
}

// HeartbeatContext returns relevant heartbeat learnings for a chat as a bullet list.
func (m *Memory) HeartbeatContext(ctx context.Context, chatID int64, budget int) string {
	if budget <= 0 {
		budget = 500
	}
	result, err := m.store.Context(ctx, agentmemory.ContextParams{
		NS:     heartbeatNamespace(chatID) + "*",
		Query:  "heartbeat patterns insights",
		Budget: budget,
	})
	if err != nil {
		slog.Warn("heartbeat context fetch failed", "error", err)
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
		NS:         namespace,
		Key:        fmt.Sprintf("reviewer-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       "semantic",
		Priority:   "high",
		Importance: 0.75, // critical flow knowledge
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
			if utf8.RuneCountInString(content) > 120 {
				content = string([]rune(content)[:120]) + "..."
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
		NS:         ns,
		Key:        key,
		Content:    content,
		Kind:       "semantic",
		Priority:   "high",
		Importance: 0.9, // system capabilities — near-identity level
		Tier:       "ltm", // system/identity content should never start as stm
	})
	return err
}

// exchangeSummarizeThreshold is the number of episodic exchanges before summarization kicks in.
const exchangeSummarizeThreshold = 10

// SummarizeExchanges consolidates old episodic exchanges into a single semantic summary.
// Keeps the most recent exchanges intact and merges older ones.
// Returns the number of exchanges consolidated, or 0 if no summarization was needed.
func (m *Memory) SummarizeExchanges(ctx context.Context, chatID int64) (int, error) {
	ns := namespace(chatID)

	// List all episodic exchanges (newest first)
	exchanges, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "episodic",
		Limit: 100,
	})
	if err != nil {
		return 0, fmt.Errorf("list exchanges: %w", err)
	}

	if len(exchanges) <= exchangeSummarizeThreshold {
		return 0, nil
	}

	// Keep the most recent 5 exchanges, summarize the rest
	keepRecent := 5
	toSummarize := exchanges[keepRecent:]

	// Build summary from oldest exchanges
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Consolidated summary of %d conversations:\n", len(toSummarize)))
	for _, ex := range toSummarize {
		// Each exchange is "User: ...\nAssistant: ..."
		// Extract just the topic/gist
		line := strings.ReplaceAll(ex.Content, "\n", " | ")
		if utf8.RuneCountInString(line) > 120 {
			line = string([]rune(line)[:120]) + "..."
		}
		sb.WriteString("- ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}

	// Store the consolidated summary
	_, err = m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        fmt.Sprintf("exchange-summary-%d", time.Now().UnixMilli()),
		Content:    sb.String(),
		Kind:       "semantic",
		Importance: 0.5,
	})
	if err != nil {
		return 0, fmt.Errorf("store summary: %w", err)
	}

	// Delete the individual exchanges that were summarized
	for _, ex := range toSummarize {
		if rmErr := m.store.Rm(ctx, agentmemory.RmParams{NS: ns, Key: ex.Key}); rmErr != nil {
			slog.Warn("failed to remove summarized exchange", "key", ex.Key, "error", rmErr)
		}
	}

	slog.Info("summarized exchanges", "chat_id", chatID, "consolidated", len(toSummarize))
	return len(toSummarize), nil
}

// RunReflect runs the memory reflect cycle to promote, decay, and prune memories.
// Should be called periodically (e.g., after heartbeat processing).
func (m *Memory) RunReflect(ctx context.Context) {
	result, err := m.store.Reflect(ctx, agentmemory.ReflectParams{})
	if err != nil {
		slog.Warn("memory reflect failed", "error", err)
		return
	}
	if result.Promoted+result.Decayed+result.Demoted+result.Deleted+result.Archived > 0 {
		slog.Info("memory reflect complete",
			"evaluated", result.MemoriesEvaluated,
			"promoted", result.Promoted,
			"decayed", result.Decayed,
			"demoted", result.Demoted,
			"deleted", result.Deleted,
			"archived", result.Archived,
		)
	}
}

// Close closes the underlying store.
func (m *Memory) Close() error {
	return m.store.Close()
}

// sanitizeKey creates a short key from content text.
func sanitizeKey(content string) string {
	// Take first ~40 chars, lowercase, replace spaces with dashes
	key := strings.ToLower(content)
	if utf8.RuneCountInString(key) > 40 {
		key = string([]rune(key)[:40])
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
