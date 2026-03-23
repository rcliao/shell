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
	AgentNS          string   // agent namespace (e.g. "agent:pikamini")
	SystemNamespaces []string // deprecated: kept for backward compat, use AgentNS + pinned
	SystemBudget     int
	GlobalNamespaces []string // deprecated: kept for backward compat, use AgentNS + tags
	GlobalBudget     int
	Budget           int
	ExchangeTTL      string // "7d", "30d"
	ExchangeMaxUser  int    // 0 = default 200
	ExchangeMaxReply int    // 0 = default 300
	MemoryDirectives bool
	DirectiveNS      string // deprecated: use AgentNS + "learning" tag
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

// SystemPrompt returns always-on context for the system prompt.
// When AgentNS is set, loads pinned memories from the agent namespace via Context().
// Falls back to the legacy List()-per-namespace approach when AgentNS is empty.
func (m *Memory) SystemPrompt(ctx context.Context, chatID int64) string {
	prof := m.profileFor(chatID)

	// New path: load pinned memories from agent namespace
	if prof.AgentNS != "" {
		return m.systemPromptFromAgent(ctx, prof)
	}

	// Legacy path: list from system namespaces
	return m.systemPromptFromNamespaces(ctx, prof)
}

// ghostSearchInstruction tells the agent to use ghost tools when context is missing.
const ghostSearchInstruction = `[Memory retrieval] Sessions rotate frequently to save tokens. If the user references something you don't have context for, use ghost_search or ghost_context (with exclude_pinned=true for deeper search) to recall it before saying you don't know.`

// systemPromptFromAgent loads pinned memories from the agent namespace via Context().
func (m *Memory) systemPromptFromAgent(ctx context.Context, prof ProfileConfig) string {
	budget := prof.SystemBudget
	if budget <= 0 {
		budget = 3000
	}

	// Context() Phase 1 loads pinned memories automatically
	result, err := m.store.Context(ctx, agentmemory.ContextParams{
		NS:     prof.AgentNS,
		Query:  "", // empty query = pinned memories only
		Budget: budget,
	})
	if err != nil {
		slog.Warn("system prompt context failed", "ns", prof.AgentNS, "error", err)
		return ghostSearchInstruction
	}
	if len(result.Memories) == 0 {
		return ghostSearchInstruction
	}

	var sb strings.Builder
	for _, mem := range result.Memories {
		sb.WriteString("- ")
		sb.WriteString(mem.Content)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(ghostSearchInstruction)
	return strings.TrimSpace(sb.String())
}

// systemPromptFromNamespaces is the legacy path: List() per system namespace.
func (m *Memory) systemPromptFromNamespaces(ctx context.Context, prof ProfileConfig) string {
	if len(prof.SystemNamespaces) == 0 {
		return ""
	}

	charBudget := prof.SystemBudget * 4
	var sb strings.Builder
	used := 0

	for _, ns := range prof.SystemNamespaces {
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

// chatTag returns the tag for a chat ID.
func chatTag(chatID int64) string {
	return fmt.Sprintf("chat:%d", chatID)
}

// legacyNamespace returns the legacy per-chat namespace (for backward compat).
func legacyNamespace(chatID int64) string {
	return fmt.Sprintf("shell:chat:%d", chatID)
}

// InjectContext fetches relevant memories and prepends them to the user message.
// When AgentNS is set, queries the agent namespace with chat tag filtering.
// Falls back to the legacy namespace-per-chat approach when AgentNS is empty.
func (m *Memory) InjectContext(ctx context.Context, chatID int64, userMsg string) string {
	prof := m.profileFor(chatID)
	var sb strings.Builder

	if prof.AgentNS != "" {
		// New path: query agent namespace with chat tag
		result, err := m.store.Context(ctx, agentmemory.ContextParams{
			NS:     prof.AgentNS,
			Query:  userMsg,
			Tags:   []string{chatTag(chatID)},
			Budget: prof.Budget,
		})
		if err != nil {
			slog.Warn("memory context fetch failed", "error", err)
		} else {
			if len(result.Memories) > 0 {
				sb.WriteString("[Relevant memories from previous conversations]\n")
				for _, mem := range result.Memories {
					sb.WriteString("- ")
					sb.WriteString(mem.Content)
					sb.WriteString("\n")
				}
				sb.WriteString("[End of memories]\n\n")
			}
			if result.CompactionSuggested {
				slog.Info("memory compaction suggested, triggering background reflect", "ns", prof.AgentNS, "skipped", result.Skipped)
				go m.RunReflect(ctx)
			}
		}
	} else {
		// Legacy path: global namespaces + legacy namespace-per-chat
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

		ns := legacyNamespace(chatID)
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

	ns := prof.AgentNS
	var tags []string
	if ns != "" {
		tags = []string{chatTag(chatID)}
	} else {
		ns = legacyNamespace(chatID)
	}

	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        fmt.Sprintf("exchange-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       "episodic",
		Tags:       tags,
		Tier:       "sensory", // raw observations — promoted to stm if accessed
		TTL:        ttl,
		Importance: 0.3, // ephemeral exchanges — low importance, will decay naturally
	})
	if err != nil {
		slog.Warn("failed to log exchange to memory", "error", err)
	}
}

// Remember stores a user-provided memory as semantic memory.
func (m *Memory) Remember(ctx context.Context, chatID int64, content string) error {
	prof := m.profileFor(chatID)
	ns := prof.AgentNS
	var tags []string
	if ns != "" {
		tags = []string{chatTag(chatID)}
	} else {
		ns = legacyNamespace(chatID)
	}

	key := sanitizeKey(content)
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        key,
		Content:    content,
		Kind:       "semantic",
		Tags:       tags,
		Priority:   "high",
		Importance: 0.8, // user-remembered facts are high importance
		Tier:       "ltm", // explicitly saved by user — skip stm
	})
	return err
}

// Forget removes a memory by key.
func (m *Memory) Forget(ctx context.Context, chatID int64, key string) error {
	prof := m.profileFor(chatID)
	ns := prof.AgentNS
	if ns == "" {
		ns = legacyNamespace(chatID)
	}
	return m.store.Rm(ctx, agentmemory.RmParams{
		NS:  ns,
		Key: key,
	})
}

// ListMemories returns a formatted debug view of all memories across related namespaces.
func (m *Memory) ListMemories(ctx context.Context, chatID int64) (string, error) {
	prof := m.profileFor(chatID)

	var sb strings.Builder
	totalCount := 0

	if prof.AgentNS != "" {
		// New path: list from agent namespace with tag filtering
		agentNS := prof.AgentNS
		tag := chatTag(chatID)

		// Peek for tier overview
		peek, err := m.store.Peek(ctx, agentNS)
		if err == nil && len(peek.MemoryCounts) > 0 {
			sb.WriteString("## Memory Overview\n\n")
			for tier, count := range peek.MemoryCounts {
				tokens := peek.TotalEstTokens[tier]
				sb.WriteString(fmt.Sprintf("- **%s**: %d memories (~%d tokens)\n", tier, count, tokens))
			}
			if peek.PinnedSummary != "" {
				sb.WriteString(fmt.Sprintf("\nPinned: %s\n", peek.PinnedSummary))
			}
			sb.WriteString("\n")
		}

		// Chat semantic memories (tagged with chat ID)
		semantic, _ := m.store.List(ctx, agentmemory.ListParams{NS: agentNS, Kind: "semantic", Tags: []string{tag}, Limit: 50})
		if len(semantic) > 0 {
			sb.WriteString("## Saved Memories\n\n")
			for _, mem := range semantic {
				sb.WriteString(fmt.Sprintf("- **%s** [%s, imp=%.1f, acc=%d] — %s\n",
					mem.Key, mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 120)))
				totalCount++
			}
			sb.WriteString("\n")
		}

		// Chat episodic memories (tagged with chat ID)
		episodic, _ := m.store.List(ctx, agentmemory.ListParams{NS: agentNS, Kind: "episodic", Tags: []string{tag}, Limit: 10})
		if len(episodic) > 0 {
			sb.WriteString("## Recent Exchanges\n\n")
			for _, mem := range episodic {
				sb.WriteString(fmt.Sprintf("- [%s, imp=%.1f, acc=%d] %s\n",
					mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 100)))
				totalCount++
			}
			sb.WriteString("\n")
		}

		// Heartbeat learnings (tagged with heartbeat + chat ID)
		hbMems, _ := m.store.List(ctx, agentmemory.ListParams{NS: agentNS, Tags: []string{"heartbeat", tag}, Limit: 20})
		if len(hbMems) > 0 {
			sb.WriteString("## Heartbeat Learnings\n\n")
			for _, mem := range hbMems {
				sb.WriteString(fmt.Sprintf("- [%s, imp=%.1f, acc=%d] %s\n",
					mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 120)))
				totalCount++
			}
			sb.WriteString("\n")
		}

		// Learnings (tagged with learning)
		learnings, _ := m.store.List(ctx, agentmemory.ListParams{NS: agentNS, Tags: []string{"learning"}, Limit: 20})
		if len(learnings) > 0 {
			sb.WriteString("## Learnings\n\n")
			for _, mem := range learnings {
				sb.WriteString(fmt.Sprintf("- **%s** [%s, imp=%.1f] — %s\n",
					mem.Key, mem.Tier, mem.Importance, truncateStr(mem.Content, 80)))
				totalCount++
			}
			sb.WriteString("\n")
		}
	} else {
		// Legacy path
		chatNS := legacyNamespace(chatID)
		hbNS := legacyHeartbeatNamespace(chatID)

		// Peek for tier overview
		peek, err := m.store.Peek(ctx, chatNS+"*")
		if err == nil && len(peek.MemoryCounts) > 0 {
			sb.WriteString("## Memory Overview\n\n")
			for tier, count := range peek.MemoryCounts {
				tokens := peek.TotalEstTokens[tier]
				sb.WriteString(fmt.Sprintf("- **%s**: %d memories (~%d tokens)\n", tier, count, tokens))
			}
			if peek.PinnedSummary != "" {
				sb.WriteString(fmt.Sprintf("\nPinned: %s\n", peek.PinnedSummary))
			}
			sb.WriteString("\n")
		}

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

		hbMems, _ := m.store.List(ctx, agentmemory.ListParams{NS: hbNS, Kind: "episodic", Limit: 20})
		if len(hbMems) > 0 {
			sb.WriteString("## Heartbeat Learnings\n\n")
			for _, mem := range hbMems {
				sb.WriteString(fmt.Sprintf("- [%s, imp=%.1f, acc=%d] %s\n",
					mem.Tier, mem.Importance, mem.AccessCount, truncateStr(mem.Content, 120)))
				totalCount++
			}
			sb.WriteString("\n")
		}

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
	if !prof.MemoryDirectives {
		return response
	}

	// Determine target namespace and tags
	ns := prof.DirectiveNS
	var tags []string
	if prof.AgentNS != "" {
		ns = prof.AgentNS
		tags = []string{"learning"}
	}
	if ns == "" {
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

		_, err := m.store.Put(ctx, agentmemory.PutParams{
			NS:         ns,
			Key:        fmt.Sprintf("learning-%d", time.Now().UnixMilli()),
			Content:    content,
			Kind:       kind,
			Tags:       tags,
			Importance: 0.7, // agent-discovered learnings
		})
		if err != nil {
			slog.Warn("failed to store memory directive", "ns", ns, "error", err)
		} else {
			slog.Info("stored memory directive", "ns", ns, "kind", kind, "len", len(content))
		}
	}

	return strings.TrimSpace(clean)
}

// StoreDirective stores a memory from an RPC call (equivalent to [remember] directive).
func (m *Memory) StoreDirective(ctx context.Context, chatID int64, content, kind string) error {
	prof := m.profileFor(chatID)
	if !prof.MemoryDirectives {
		return fmt.Errorf("memory directives disabled for chat %d", chatID)
	}

	ns := prof.DirectiveNS
	var tags []string
	if prof.AgentNS != "" {
		ns = prof.AgentNS
		tags = []string{"learning"}
	}
	if ns == "" {
		return fmt.Errorf("no target namespace configured for chat %d", chatID)
	}
	if kind == "" {
		kind = "semantic"
	}

	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        fmt.Sprintf("learning-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       kind,
		Tags:       tags,
		Importance: 0.7,
	})
	return err
}

// legacyHeartbeatNamespace returns the legacy per-chat heartbeat learning namespace.
func legacyHeartbeatNamespace(chatID int64) string {
	return fmt.Sprintf("shell:heartbeat:%d", chatID)
}

// RecentExchanges returns the last N episodic exchanges for a chat.
func (m *Memory) RecentExchanges(ctx context.Context, chatID int64, limit int) []string {
	if limit <= 0 {
		limit = 10
	}
	prof := m.profileFor(chatID)
	var memories []agentmemory.Memory
	var err error
	if prof.AgentNS != "" {
		memories, err = m.store.List(ctx, agentmemory.ListParams{
			NS:    prof.AgentNS,
			Kind:  "episodic",
			Tags:  []string{chatTag(chatID)},
			Limit: limit,
		})
	} else {
		memories, err = m.store.List(ctx, agentmemory.ListParams{
			NS:    legacyNamespace(chatID),
			Kind:  "episodic",
			Limit: limit,
		})
	}
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

// StoreHeartbeatLearning stores a heartbeat learning as episodic memory.
// Starts as stm — will be promoted to ltm if accessed frequently.
// Prunes oldest entries beyond maxHeartbeatLearnings.
func (m *Memory) StoreHeartbeatLearning(ctx context.Context, chatID int64, content string) error {
	prof := m.profileFor(chatID)
	ns := prof.AgentNS
	var tags []string
	if ns != "" {
		tags = []string{"heartbeat", chatTag(chatID)}
	} else {
		ns = legacyHeartbeatNamespace(chatID)
	}

	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        fmt.Sprintf("hb-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       "episodic",
		Tags:       tags,
		Priority:   "high",
		Importance: 0.7, // heartbeat learnings — moderately important, self-discovered
	})
	if err != nil {
		return err
	}

	// Prune oldest learnings beyond cap
	var all []agentmemory.Memory
	var listErr error
	if prof.AgentNS != "" {
		all, listErr = m.store.List(ctx, agentmemory.ListParams{
			NS:    ns,
			Tags:  []string{"heartbeat", chatTag(chatID)},
			Limit: maxHeartbeatLearnings + 20,
		})
	} else {
		all, listErr = m.store.List(ctx, agentmemory.ListParams{
			NS:    ns,
			Kind:  "episodic",
			Limit: maxHeartbeatLearnings + 20,
		})
	}
	if listErr != nil {
		slog.Warn("heartbeat learning prune list failed", "error", listErr)
		return nil
	}
	if len(all) > maxHeartbeatLearnings {
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
	prof := m.profileFor(chatID)
	var result *agentmemory.ContextResult
	var err error
	if prof.AgentNS != "" {
		result, err = m.store.Context(ctx, agentmemory.ContextParams{
			NS:     prof.AgentNS,
			Query:  "heartbeat patterns insights",
			Tags:   []string{"heartbeat", chatTag(chatID)},
			Budget: budget,
		})
	} else {
		result, err = m.store.Context(ctx, agentmemory.ContextParams{
			NS:     legacyHeartbeatNamespace(chatID) + "*",
			Query:  "heartbeat patterns insights",
			Budget: budget,
		})
	}
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

// StoreReviewerLearning stores a reviewer learning as procedural memory.
// Flow knowledge is how-to knowledge — procedural by nature.
// No TTL — critical flow knowledge shouldn't expire.
func (m *Memory) StoreReviewerLearning(ctx context.Context, namespace, content string) error {
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         namespace,
		Key:        fmt.Sprintf("reviewer-%d", time.Now().UnixMilli()),
		Content:    content,
		Kind:       "procedural",
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
	prof := m.profileFor(chatID)
	ns := prof.AgentNS
	var tags []string
	if ns != "" {
		tags = []string{chatTag(chatID)}
	} else {
		ns = legacyNamespace(chatID)
	}

	semantic, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "semantic",
		Tags:  tags,
		Limit: 50,
	})
	if err != nil {
		return "", nil, err
	}

	episodic, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "episodic",
		Tags:  tags,
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

// SeedCapability stores a capability doc as a pinned procedural memory.
// Uses Put with the same NS+key, so repeated calls are idempotent (upserts).
// When agentNS is provided, stores in that namespace with "capabilities" tag and pinned=true.
// Falls back to the provided namespace with legacy tier for backward compat.
func (m *Memory) SeedCapability(ctx context.Context, agentNS, key, content string) error {
	if agentNS != "" {
		_, err := m.store.Put(ctx, agentmemory.PutParams{
			NS:         agentNS,
			Key:        key,
			Content:    content,
			Kind:       "procedural",
			Tags:       []string{"capabilities"},
			Priority:   "high",
			Importance: 0.9,
			Tier:       "ltm",
			Pinned:     true,
		})
		return err
	}
	// Legacy fallback
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         "shell:capabilities",
		Key:        key,
		Content:    content,
		Kind:       "procedural",
		Priority:   "high",
		Importance: 0.9,
		Tier:       "ltm",
		Pinned:     true,
	})
	return err
}

// SeedNamespace is deprecated — use SeedCapability instead.
// Kept for backward compatibility.
func (m *Memory) SeedNamespace(ctx context.Context, ns, key, content string) error {
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        key,
		Content:    content,
		Kind:       "procedural",
		Priority:   "high",
		Importance: 0.9,
		Tier:       "ltm",
		Pinned:     true,
	})
	return err
}

// AgentNS returns the ghost namespace for a chat's profile.
func (m *Memory) AgentNS(chatID int64) string {
	return m.profileFor(chatID).AgentNS
}

// HasIdentity checks whether the agent has any pinned identity memories.
func (m *Memory) HasIdentity(ctx context.Context, chatID int64) bool {
	prof := m.profileFor(chatID)
	if prof.AgentNS == "" {
		return true // legacy mode: skip onboarding
	}
	mems, err := m.store.List(ctx, agentmemory.ListParams{
		NS:   prof.AgentNS,
		Tags: []string{"identity"},
	})
	if err != nil {
		slog.Warn("identity check failed", "error", err)
		return true // fail open: don't block on errors
	}
	return len(mems) > 0
}

// StoreIdentity stores a pinned identity memory for the agent.
func (m *Memory) StoreIdentity(ctx context.Context, chatID int64, key, content string) error {
	prof := m.profileFor(chatID)
	ns := prof.AgentNS
	if ns == "" {
		return fmt.Errorf("identity storage requires AgentNS")
	}
	_, err := m.store.Put(ctx, agentmemory.PutParams{
		NS:         ns,
		Key:        "identity-" + sanitizeKey(key),
		Content:    content,
		Kind:       "semantic",
		Tags:       []string{"identity"},
		Priority:   "critical",
		Importance: 1.0,
		Tier:       "ltm",
		Pinned:     true,
	})
	return err
}

// ListIdentity returns all identity memories for display.
func (m *Memory) ListIdentity(ctx context.Context, chatID int64) (string, error) {
	prof := m.profileFor(chatID)
	if prof.AgentNS == "" {
		return "Identity not configured (legacy mode).", nil
	}
	mems, err := m.store.List(ctx, agentmemory.ListParams{
		NS:   prof.AgentNS,
		Tags: []string{"identity"},
	})
	if err != nil {
		return "", err
	}
	if len(mems) == 0 {
		return "No identity defined yet. Send a message to start onboarding.", nil
	}

	var sb strings.Builder
	sb.WriteString("## Identity\n\n")
	for _, mem := range mems {
		sb.WriteString("- ")
		sb.WriteString(mem.Content)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// ArchiveIdentity moves all identity memories to an archive namespace and removes them
// from the active agent namespace, returning the count of archived memories.
func (m *Memory) ArchiveIdentity(ctx context.Context, chatID int64) (int, error) {
	prof := m.profileFor(chatID)
	if prof.AgentNS == "" {
		return 0, fmt.Errorf("identity archive requires AgentNS")
	}
	mems, err := m.store.List(ctx, agentmemory.ListParams{
		NS:   prof.AgentNS,
		Tags: []string{"identity"},
	})
	if err != nil {
		return 0, err
	}
	if len(mems) == 0 {
		return 0, nil
	}

	archiveNS := prof.AgentNS + ":identity-archive"
	archiveKey := fmt.Sprintf("archive-%d", time.Now().Unix())

	// Consolidate all identity memories into one archive entry.
	var sb strings.Builder
	for _, mem := range mems {
		sb.WriteString(mem.Content)
		sb.WriteString("\n")
	}
	_, err = m.store.Put(ctx, agentmemory.PutParams{
		NS:       archiveNS,
		Key:      archiveKey,
		Content:  strings.TrimSpace(sb.String()),
		Kind:     "semantic",
		Tags:     []string{"identity-archive"},
		Priority: "low",
		Tier:     "ltm",
	})
	if err != nil {
		return 0, fmt.Errorf("archiving identity: %w", err)
	}

	// Remove originals.
	for _, mem := range mems {
		m.store.Rm(ctx, agentmemory.RmParams{NS: prof.AgentNS, Key: mem.Key})
	}

	return len(mems), nil
}

// exchangeSummarizeThreshold is the number of episodic exchanges before summarization kicks in.
const exchangeSummarizeThreshold = 10

// SummarizeExchanges consolidates old episodic exchanges into a single semantic summary.
// Keeps the most recent exchanges intact and merges older ones.
// Returns the number of exchanges consolidated, or 0 if no summarization was needed.
func (m *Memory) SummarizeExchanges(ctx context.Context, chatID int64) (int, error) {
	prof := m.profileFor(chatID)
	ns := prof.AgentNS
	var tags []string
	if ns != "" {
		tags = []string{chatTag(chatID)}
	} else {
		ns = legacyNamespace(chatID)
	}

	// List all episodic exchanges (newest first)
	exchanges, err := m.store.List(ctx, agentmemory.ListParams{
		NS:    ns,
		Kind:  "episodic",
		Tags:  tags,
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

	// Consolidate: create summary node with contains edges to source exchanges.
	// Children are suppressed in context assembly but preserved for drill-down.
	summaryKey := fmt.Sprintf("exchange-summary-%d", time.Now().UnixMilli())
	var sourceKeys []string
	for _, ex := range toSummarize {
		sourceKeys = append(sourceKeys, ex.Key)
	}

	if len(sourceKeys) >= 2 {
		_, err = m.store.Consolidate(ctx, agentmemory.ConsolidateParams{
			NS:         ns,
			SummaryKey: summaryKey,
			Content:    sb.String(),
			SourceKeys: sourceKeys,
			Kind:       "semantic",
			Importance: 0.5,
			Tags:       tags,
		})
		if err != nil {
			// Fallback: store as flat summary if consolidation fails
			slog.Warn("consolidate exchanges failed, falling back to flat summary", "error", err)
			_, err = m.store.Put(ctx, agentmemory.PutParams{
				NS:         ns,
				Key:        summaryKey,
				Content:    sb.String(),
				Kind:       "semantic",
				Tags:       tags,
				Importance: 0.5,
			})
			if err != nil {
				return 0, fmt.Errorf("store summary: %w", err)
			}
		}
	} else {
		// Less than 2 keys — can't consolidate, store flat
		_, err = m.store.Put(ctx, agentmemory.PutParams{
			NS:         ns,
			Key:        summaryKey,
			Content:    sb.String(),
			Kind:       "semantic",
			Tags:       tags,
			Importance: 0.5,
		})
		if err != nil {
			return 0, fmt.Errorf("store summary: %w", err)
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
