package skill

import (
	"fmt"
	"sort"
	"strings"
)

// Token budgets for the 3-tier catalog.
// Core skills are always full body (no budget). Hot and lazy each get a cap;
// over-budget skills at the hot tier are auto-demoted to lazy for this render.
const (
	HotTierBudget  = 1000 // tokens — full bodies pre-loaded in system prompt
	LazyTierBudget = 1000 // tokens — one-liner catalog entries
)

// EstimateTokens is a cheap approximation used for budget math.
// Uses the chars/4 heuristic — good enough for budget gating; the retro loop
// can decide to cut skills long before the actual tokenizer disagrees.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// Registry holds loaded skills and provides merged configuration.
type Registry struct {
	skills       []*Skill
	byName       map[string]*Skill
	allowedTools []string
}

// NewRegistry creates a registry from the given skills.
// Duplicate names are resolved by last-wins (project skills override global).
func NewRegistry(skills []*Skill) *Registry {
	r := &Registry{
		byName: make(map[string]*Skill, len(skills)),
	}

	seen := make(map[string]bool)
	var toolSet []string

	for _, s := range skills {
		r.byName[s.Name] = s
		r.skills = append(r.skills, s)

		for _, t := range s.AllowedTools {
			if !seen[t] {
				seen[t] = true
				toolSet = append(toolSet, t)
			}
		}
	}

	r.allowedTools = toolSet
	return r
}

// All returns all loaded skills.
func (r *Registry) All() []*Skill {
	return r.skills
}

// SystemPrompt returns the skills listing for the system prompt.
// Includes name, description, directory path, and full body so the agent
// knows how to invoke scripts by absolute path.
func (r *Registry) SystemPrompt() string {
	if len(r.skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Available Skills\n\n")
	sb.WriteString("The following skills are available. Skills with a scripts/ directory contain executables you can run via Bash using the full path.\n\n")
	sb.WriteString("**IMPORTANT: Artifact markers.** When a skill script outputs lines matching `[artifact type=\"...\" path=\"...\" caption=\"...\"]`, ")
	sb.WriteString("you MUST include them verbatim in your response text. The bridge parses these markers to deliver binary content (images, files) to the user. ")
	sb.WriteString("Do not omit, paraphrase, or summarize artifact markers.\n\n")
	for _, s := range r.skills {
		sb.WriteString("### ")
		sb.WriteString(s.Name)
		sb.WriteString("\n")
		sb.WriteString(s.Description)
		sb.WriteString("\n")
		sb.WriteString("- Dir: `")
		sb.WriteString(s.Dir)
		sb.WriteString("`\n")
		if s.ScriptsDir != "" {
			sb.WriteString("- Scripts: `")
			sb.WriteString(s.ScriptsDir)
			sb.WriteString("/`\n")
		}
		if s.Body != "" {
			sb.WriteString("\n")
			sb.WriteString(s.Body)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// CatalogPrompt returns the skills section for the system prompt using the
// 3-tier layout:
//
//	core: full body always inlined, no budget.
//	hot:  full body inlined subject to HotTierBudget. Overflow auto-demotes
//	      the lowest-priority hot skills to lazy for this render.
//	lazy: compact one-liner catalog, capped at LazyTierBudget. Overflow
//	      truncates with a pointer to where the rest lives on disk.
//
// Hot demotion / lazy truncation are deterministic (stable sort by name) so
// the prompt stays cache-friendly across renders; the retro loop is where
// usage-based reordering happens.
func (r *Registry) CatalogPrompt() string {
	if len(r.skills) == 0 {
		return ""
	}

	// Split by tier. Sort each bucket by name for stable render order.
	var core, hot, lazy []*Skill
	for _, s := range r.skills {
		switch s.Tier {
		case TierCore:
			core = append(core, s)
		case TierHot:
			hot = append(hot, s)
		default:
			lazy = append(lazy, s)
		}
	}
	sort.Slice(core, func(i, j int) bool { return core[i].Name < core[j].Name })
	sort.Slice(hot, func(i, j int) bool { return hot[i].Name < hot[j].Name })
	sort.Slice(lazy, func(i, j int) bool { return lazy[i].Name < lazy[j].Name })

	// Pack hot tier against budget; demote overflow into lazy.
	var hotFitted []*Skill
	hotUsed := 0
	for _, s := range hot {
		cost := EstimateTokens(renderHotBody(s))
		if hotUsed+cost > HotTierBudget {
			lazy = append(lazy, s) // demote — catalog entry still visible
			continue
		}
		hotFitted = append(hotFitted, s)
		hotUsed += cost
	}
	// Re-sort lazy after any demotions.
	sort.Slice(lazy, func(i, j int) bool { return lazy[i].Name < lazy[j].Name })

	var sb strings.Builder
	sb.WriteString("\n\n## Skills\n\n")
	sb.WriteString("**Artifact markers:** When a skill script outputs `[artifact type=\"...\" path=\"...\" caption=\"...\"]`, ")
	sb.WriteString("include them verbatim in your response — the bridge sends them as images/files to the user.\n\n")

	// Core tier — always full body, no budget.
	for _, s := range core {
		sb.WriteString(renderFullSkill(s))
	}

	// Hot tier — full bodies within budget.
	if len(hotFitted) > 0 {
		sb.WriteString("### Hot skills\n\n")
		for _, s := range hotFitted {
			sb.WriteString(renderFullSkill(s))
		}
	}

	// Lazy tier — compact catalog, capped.
	if len(lazy) > 0 {
		sb.WriteString("### Lazy skills (Read the SKILL.md before invoking)\n\n")
		lazyUsed := 0
		shown := 0
		for _, s := range lazy {
			line := renderLazyLine(s)
			cost := EstimateTokens(line)
			if lazyUsed+cost > LazyTierBudget {
				sb.WriteString(fmt.Sprintf("- _(%d more skills — see `%s` directory)_\n", len(lazy)-shown, skillRootHint(s)))
				break
			}
			sb.WriteString(line)
			lazyUsed += cost
			shown++
		}
	}

	return sb.String()
}

// renderFullSkill emits the heading + description + scripts path + body.
// Shared by core and hot tiers.
func renderFullSkill(s *Skill) string {
	var sb strings.Builder
	sb.WriteString("### ")
	sb.WriteString(s.Name)
	if s.Version != "" {
		sb.WriteString(" (")
		sb.WriteString(s.Version)
		sb.WriteString(")")
	}
	sb.WriteString("\n")
	sb.WriteString(s.Description)
	sb.WriteString("\n")
	if s.ScriptsDir != "" {
		sb.WriteString("Scripts: `")
		sb.WriteString(s.ScriptsDir)
		sb.WriteString("/`\n")
	}
	if s.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(s.Body)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	return sb.String()
}

// renderHotBody returns the same text as renderFullSkill; separated so the
// budget check and the actual render can diverge later (e.g. add truncation
// markers) without drift.
func renderHotBody(s *Skill) string {
	return renderFullSkill(s)
}

// renderLazyLine emits a single compact catalog entry plus the on-disk path
// so the agent knows where to Read the full SKILL.md when it decides to use
// this skill.
func renderLazyLine(s *Skill) string {
	var sb strings.Builder
	sb.WriteString("- **")
	sb.WriteString(s.Name)
	sb.WriteString("**: ")
	sb.WriteString(s.Description)
	if s.Usage != "" {
		sb.WriteString(" — Usage: `")
		sb.WriteString(s.Usage)
		sb.WriteString("`")
	}
	sb.WriteString("  [`")
	sb.WriteString(s.Dir)
	sb.WriteString("/SKILL.md`]\n")
	return sb.String()
}

// skillRootHint returns the parent skills directory for the "see more" message
// when the lazy catalog overflows.
func skillRootHint(s *Skill) string {
	if s.SkillRoot != "" {
		// Parent of the skill folder is the skills/ directory.
		if idx := strings.LastIndex(s.SkillRoot, "/"); idx > 0 {
			return s.SkillRoot[:idx]
		}
	}
	return "skills/"
}

// FullPrompt returns the full body for a single skill by name.
// Returns an error if the skill is not found.
func (r *Registry) FullPrompt(name string) (string, error) {
	s, ok := r.byName[name]
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}

	var sb strings.Builder
	sb.WriteString("### ")
	sb.WriteString(s.Name)
	sb.WriteString("\n")
	sb.WriteString(s.Description)
	sb.WriteString("\n")
	sb.WriteString("- Dir: `")
	sb.WriteString(s.Dir)
	sb.WriteString("`\n")
	if s.ScriptsDir != "" {
		sb.WriteString("- Scripts: `")
		sb.WriteString(s.ScriptsDir)
		sb.WriteString("/`\n")
	}
	if s.Body != "" {
		sb.WriteString("\n")
		sb.WriteString(s.Body)
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

// AllowedTools returns the merged allowed-tools from all skills.
func (r *Registry) AllowedTools() []string {
	return r.allowedTools
}

// Get returns a skill by name, or nil if not found.
func (r *Registry) Get(name string) *Skill {
	return r.byName[name]
}

// Has returns true if a skill with the given name is loaded.
func (r *Registry) Has(name string) bool {
	_, ok := r.byName[name]
	return ok
}
