package skill

import (
	"fmt"
	"sort"
	"strings"
)

// Token budgets for the 3-tier catalog.
// Core skills are always full body (no budget). Hot and lazy each get a cap;
// over-budget skills at the hot tier are auto-demoted to lazy for this render.
//
// The hot budget used to be 1,000 tokens for ALL hot skills together, packed
// alphabetically — with five hot skills totalling ~5,900 tokens only the
// first (google) ever loaded, and the rest, including the agent's own
// meal-memo rules, were dropped without a log line. A hot skill now
// contributes only its `<!-- hot -->` rules section when it has one.
const (
	HotTierBudget  = 3200 // tokens — hot rules sections pre-loaded in system prompt
	LazyTierBudget = 1000 // tokens — one-liner catalog entries
)

// HotStart and HotEnd delimit the part of a SKILL.md body that must always be
// in the prompt (the rules that have to hold without the agent reading the
// file). Everything else stays on disk behind a pointer.
const (
	HotStart = "<!-- hot -->"
	HotEnd   = "<!-- /hot -->"
)

// EstimateTokens is a cheap approximation used for budget math: ASCII at
// ~4 characters per token, any other rune (CJK, emoji) at ~1 token each.
// Byte length/4 undercounted Chinese three-fold per character.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

// hotSection returns the marked always-loaded section of a body, if any.
func hotSection(body string) (string, bool) {
	i := strings.Index(body, HotStart)
	if i < 0 {
		return "", false
	}
	rest := body[i+len(HotStart):]
	if j := strings.Index(rest, HotEnd); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest), true
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
	sort.Slice(lazy, func(i, j int) bool { return lazy[i].Name < lazy[j].Name })

	hotFitted, demoted := packHot(hot)
	demotedSet := map[string]bool{}
	for _, s := range demoted {
		demotedSet[s.Name] = true
		lazy = append(lazy, s) // demote — catalog entry still visible, and says so
	}
	// Re-sort lazy after any demotions.
	sort.Slice(lazy, func(i, j int) bool { return lazy[i].Name < lazy[j].Name })

	var sb strings.Builder
	sb.WriteString("\n\n## Skills\n\n")
	sb.WriteString("**Skills are Bash scripts you invoke by file path**, NOT native MCP tools. ")
	sb.WriteString("Each entry below shows a `Usage:` one-liner — that is the invocation. ")
	sb.WriteString("Do not search for skills in your tool list; they live in the filesystem at the paths shown. ")
	sb.WriteString("Always invoke scripts by ABSOLUTE path — relative `scripts/...` forms resolve against your working directory and will fail. ")
	sb.WriteString("If a one-liner isn't enough detail, read the skill's SKILL.md at the path shown.\n\n")
	sb.WriteString("**When a local skill and a remote/connector MCP tool can do the same job, use the skill** — ")
	sb.WriteString("local scripts run in seconds; remote connector tools have been measured at 90s+ for single operations.\n\n")
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
			sb.WriteString(renderHotBody(s))
		}
	}

	// Lazy tier — compact catalog, capped.
	if len(lazy) > 0 {
		sb.WriteString("### Lazy skills (use the `Usage:` line; read the SKILL.md path for more detail)\n\n")
		lazyUsed := 0
		shown := 0
		for _, s := range lazy {
			line := renderLazyLine(s)
			if demotedSet[s.Name] {
				line = strings.Replace(line, "**: ", "** (hot, NOT loaded — over the prompt budget; read its SKILL.md before using it): ", 1)
			}
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

// renderHotBody is what a hot skill costs and contributes: its marked rules
// section plus a pointer to the full file, or the whole body when unmarked.
func renderHotBody(s *Skill) string {
	sec, ok := hotSection(s.Body)
	if !ok {
		return renderFullSkill(s)
	}
	cp := *s
	cp.Body = sec + "\n\n_Full instructions: `" + s.Dir + "/SKILL.md` — read it when the rules above are not enough._"
	return renderFullSkill(&cp)
}

// packHot fits hot skills into HotTierBudget: the agent's own skills first
// (its specialization), then shared ones, each group by name — deterministic,
// so the prompt stays cache-stable. Returns what fits and what was demoted.
func packHot(hot []*Skill) (fitted, demoted []*Skill) {
	sorted := append([]*Skill(nil), hot...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Own != sorted[j].Own {
			return sorted[i].Own
		}
		return sorted[i].Name < sorted[j].Name
	})
	used := 0
	for _, s := range sorted {
		cost := EstimateTokens(renderHotBody(s))
		if used+cost > HotTierBudget {
			demoted = append(demoted, s)
			continue
		}
		fitted = append(fitted, s)
		used += cost
	}
	return fitted, demoted
}

// Demoted reports hot skills that do not fit the budget and will render as
// catalog lines instead — the silent failure this used to be. Names with
// their estimated cost, e.g. "meal-memo (2228 tokens)".
func (r *Registry) Demoted() []string {
	var hot []*Skill
	for _, s := range r.skills {
		if s.Tier == TierHot {
			hot = append(hot, s)
		}
	}
	_, demoted := packHot(hot)
	out := make([]string, 0, len(demoted))
	for _, s := range demoted {
		out = append(out, fmt.Sprintf("%s (%d tokens)", s.Name, EstimateTokens(renderHotBody(s))))
	}
	return out
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
		// Usage lines are authored with relative `scripts/...` paths, but the
		// subprocess runs from work_dir, not the skill dir — agents were
		// observed cd-ing into the wrong repo to satisfy the relative path.
		// Absolutize at render time so the one-liner is copy-paste runnable.
		if strings.HasPrefix(s.Usage, "scripts/") {
			sb.WriteString(s.Dir)
			sb.WriteString("/")
		}
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

// HotTokens is what a hot skill costs against HotTierBudget as rendered.
func HotTokens(s *Skill) int { return EstimateTokens(renderHotBody(s)) }
