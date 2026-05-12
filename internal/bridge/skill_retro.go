package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/skill"
)

// retroStaleThreshold governs how long a skill can sit unused before the
// retro flags it as a retirement/demotion candidate.
const retroStaleThreshold = 14 * 24 * time.Hour

// buildSkillRetroBlock returns the deep-reflect "skill inventory retro"
// section, or an empty string if the skill system is disabled / there are no
// skills to reason about. The agent reads this during deep reflection and
// decides whether to promote, demote, retire, or author new skills.
//
// The output is intentionally compact — verbose reflection prompts already
// saturate attention; the retro just gives ground truth (usage stats) and
// an action menu, then gets out of the way.
func (b *Bridge) buildSkillRetroBlock() string {
	if b.skills == nil {
		return ""
	}
	all := b.skills.All()
	if len(all) == 0 {
		return ""
	}

	// Split into hot/lazy buckets — retro is only useful for non-core skills
	// since core is a global platform concern, not an agent-authored concern.
	var hot, lazy []*skill.Skill
	for _, s := range all {
		switch s.Tier {
		case skill.TierHot:
			hot = append(hot, s)
		case skill.TierLazy:
			lazy = append(lazy, s)
		}
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].Name < hot[j].Name })
	sort.Slice(lazy, func(i, j int) bool { return lazy[i].Name < lazy[j].Name })

	// Rollup stats for each non-core skill. Skip if usage log missing.
	type row struct {
		s     *skill.Skill
		stats skill.UsageStats
		stale bool
	}
	var hotRows, lazyRows []row
	for _, s := range hot {
		st, _ := skill.Rollup(s)
		hotRows = append(hotRows, row{s: s, stats: st, stale: isStale(st)})
	}
	for _, s := range lazy {
		st, _ := skill.Rollup(s)
		lazyRows = append(lazyRows, row{s: s, stats: st, stale: isStale(st)})
	}

	// Hot budget used — honest count so the agent can see the slack.
	hotTokens := 0
	for _, r := range hotRows {
		hotTokens += skill.EstimateTokens(renderFullSkillInline(r.s))
	}

	var playgroundNotes []string
	// Per-agent skills dir lives at the parent of any loaded per-agent skill.
	// We can't derive it from the registry alone, so scan the first hot/lazy
	// SkillRoot's parent. Skip if no per-agent skills loaded yet.
	skillsDir := pickPerAgentSkillsDir(all)
	if skillsDir != "" {
		playgroundNotes = scanPlayground(filepath.Join(skillsDir, "playground"))
	}

	var sb strings.Builder
	sb.WriteString("\n---\n**[Skill Inventory Retro]**\n")
	sb.WriteString(fmt.Sprintf("Hot budget: ~%d / %d tokens. Cap is load-bearing — graduate carefully.\n\n",
		hotTokens, skill.HotTierBudget))

	sb.WriteString("Hot skills (full body in system prompt):\n")
	if len(hotRows) == 0 {
		sb.WriteString("  _(none — the hot tier is empty. Promote a lazy skill if one is proving itself.)_\n")
	} else {
		for _, r := range hotRows {
			writeSkillRow(&sb, r.s, r.stats, r.stale, true)
		}
	}
	sb.WriteString("\n")

	sb.WriteString("Lazy skills (catalog entry, body Read on demand):\n")
	if len(lazyRows) == 0 {
		sb.WriteString("  _(none)_\n")
	} else {
		for _, r := range lazyRows {
			writeSkillRow(&sb, r.s, r.stats, r.stale, false)
		}
	}
	sb.WriteString("\n")

	if len(playgroundNotes) > 0 {
		sb.WriteString("Playground candidates (graduate if proven):\n")
		for _, n := range playgroundNotes {
			sb.WriteString("  - ")
			sb.WriteString(n)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	if len(hotRows) == 0 && len(lazyRows) == 0 {
		sb.WriteString("**Zero agent-authored skills.** Only platform/core skills are loaded — you have authored nothing yourself yet.\n")
		sb.WriteString("Look at recurring procedures from your last 7 days (response patterns you repeat, multi-step workflows, formatting conventions you've converged on) and draft one as a playground skill:\n")
		if skillsDir != "" {
			sb.WriteString(fmt.Sprintf("  - Create `%s/playground/<name>/SKILL.md` with frontmatter (name, description, tier: lazy) and a short body.\n", skillsDir))
		} else {
			sb.WriteString("  - Create `<your-skills-dir>/playground/<name>/SKILL.md` with frontmatter (name, description, tier: lazy) and a short body.\n")
		}
		sb.WriteString("  - Skills are how procedural knowledge crystallizes — without them, every reflection cycle starts from scratch.\n\n")
	}
	sb.WriteString("Action menu — for each skill, consider: **keep / promote-to-hot / demote-to-lazy / retire / graduate-from-playground / author-new**.\n")
	sb.WriteString("- Promotions/demotions: edit the skill's `tier:` frontmatter field (or move playground -> skills/<name>/v1/).\n")
	sb.WriteString("- Retire: move `skills/<name>/` to `skills/.archive/`. USAGE.jsonl history stays intact.\n")
	sb.WriteString("- New version: create `skills/<name>/v2/SKILL.md`, leave ACTIVE on v1 until v2 proves out.\n")
	sb.WriteString("- Base decisions on USAGE.jsonl stats (shown above), NOT on how clever the skill sounds — productivity theater wastes the hot budget.\n")
	sb.WriteString("- A compact inventory digest is auto-pinned to memory (key `skill-inventory`) after this reflect so your tools survive session rotation. If you make a significant change, also ghost_put a brief rationale memory tagged `skill:<name>` so the **why** is preserved alongside the what.\n")
	return sb.String()
}

func isStale(st skill.UsageStats) bool {
	if st.Runs == 0 {
		// No runs ever is stale after the skill has existed a while, but we
		// don't know creation time here; treat as stale conservatively so the
		// agent at least considers retiring unused skills.
		return true
	}
	if st.LastRun.IsZero() {
		return true
	}
	return time.Since(st.LastRun) > retroStaleThreshold
}

func writeSkillRow(sb *strings.Builder, s *skill.Skill, st skill.UsageStats, stale, isHot bool) {
	sb.WriteString("  - `")
	sb.WriteString(s.Name)
	if s.Version != "" {
		sb.WriteString("@")
		sb.WriteString(s.Version)
	}
	sb.WriteString("` — ")
	if st.Runs == 0 {
		sb.WriteString("0 runs ever")
	} else {
		okRate := 100 * st.Successes / st.Runs
		sb.WriteString(fmt.Sprintf("%d runs total (%d in last 7d), %d%% success", st.Runs, st.Invocations7, okRate))
		if st.AvgDuration > 0 {
			sb.WriteString(fmt.Sprintf(", avg %dms", st.AvgDuration.Milliseconds()))
		}
	}
	if stale {
		if isHot {
			sb.WriteString("  ⚠ stale — consider demoting or retiring")
		} else {
			sb.WriteString("  ⚠ stale — consider retiring")
		}
	}
	sb.WriteString("\n")
}

// renderFullSkillInline mirrors skill.renderFullSkill for token accounting.
// Duplicated here to avoid exporting an internal helper; cheap to maintain.
func renderFullSkillInline(s *skill.Skill) string {
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

// pickPerAgentSkillsDir returns the first skill's SkillRoot parent whose path
// looks like ~/.shell/agents/<name>/skills/. This lets us find the
// per-agent skills dir without plumbing it through the bridge constructor.
func pickPerAgentSkillsDir(skills []*skill.Skill) string {
	for _, s := range skills {
		root := s.SkillRoot
		if root == "" {
			root = s.Dir
		}
		if root == "" {
			continue
		}
		parent := filepath.Dir(root)
		if strings.Contains(parent, "/.shell/agents/") && strings.HasSuffix(parent, "/skills") {
			return parent
		}
	}
	return ""
}

// buildSkillInventoryDigest returns a compact, pinned-memory-friendly digest
// of the agent's hot + lazy skills — essentially "what tools I've authored
// and how they've been performing." Omits core skills (platform, not agent-
// authored). Returns empty string if there are no agent-authored skills.
//
// The digest is intended to be stored as a single pinned memory so it
// survives session rotation and sits in Channel A across future generations
// as an identity anchor: "these are the tools I've built for myself."
func (b *Bridge) buildSkillInventoryDigest() string {
	if b.skills == nil {
		return ""
	}
	all := b.skills.All()
	if len(all) == 0 {
		return ""
	}

	var hot, lazy []*skill.Skill
	for _, s := range all {
		switch s.Tier {
		case skill.TierHot:
			hot = append(hot, s)
		case skill.TierLazy:
			lazy = append(lazy, s)
		}
	}
	if len(hot) == 0 && len(lazy) == 0 {
		return ""
	}
	sort.Slice(hot, func(i, j int) bool { return hot[i].Name < hot[j].Name })
	sort.Slice(lazy, func(i, j int) bool { return lazy[i].Name < lazy[j].Name })

	var sb strings.Builder
	sb.WriteString("Skills I've authored (as of ")
	sb.WriteString(time.Now().UTC().Format("2006-01-02"))
	sb.WriteString("):\n")

	writeDigestLine := func(s *skill.Skill, tier string) {
		stats, _ := skill.Rollup(s)
		sb.WriteString("- ")
		sb.WriteString(s.Name)
		if s.Version != "" {
			sb.WriteString("@")
			sb.WriteString(s.Version)
		}
		sb.WriteString(" [")
		sb.WriteString(tier)
		if stats.Runs > 0 {
			sb.WriteString(fmt.Sprintf(", %dr/%d%%", stats.Runs, 100*stats.Successes/stats.Runs))
		} else {
			sb.WriteString(", unused")
		}
		sb.WriteString("] ")
		// One-line description — truncate to keep the digest compact.
		desc := strings.ReplaceAll(s.Description, "\n", " ")
		if len(desc) > 80 {
			desc = desc[:77] + "..."
		}
		sb.WriteString(desc)
		sb.WriteString("\n")
	}

	for _, s := range hot {
		writeDigestLine(s, "hot")
	}
	for _, s := range lazy {
		writeDigestLine(s, "lazy")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// refreshSkillInventoryMemory writes the current digest to pinned memory.
// Called from the deep-reflect path so the digest stays current. Safe to
// call when memory is disabled or the profile has no agent namespace —
// in those cases it silently no-ops. Errors are logged, not returned;
// failing to refresh the digest must never break a heartbeat.
func (b *Bridge) refreshSkillInventoryMemory(ctx context.Context, chatID int64) {
	if b.memory == nil {
		return
	}
	if b.memory.AgentNS(chatID) == "" {
		return
	}
	digest := b.buildSkillInventoryDigest()
	if digest == "" {
		return
	}
	if err := b.memory.StoreSkillInventory(ctx, chatID, digest); err != nil {
		slog.Warn("skill inventory refresh failed", "chat_id", chatID, "error", err)
	}
}

// scanPlayground returns short notes on directories under playground/ that
// look like graduation candidates (contain a SKILL.md). Absent playground
// returns nil silently.
func scanPlayground(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var notes []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mdPath := filepath.Join(dir, e.Name(), "SKILL.md")
		if _, err := os.Stat(mdPath); err != nil {
			notes = append(notes, fmt.Sprintf("`%s` (no SKILL.md yet — draft in progress)", e.Name()))
			continue
		}
		notes = append(notes, fmt.Sprintf("`%s` (has SKILL.md — ready to graduate?)", e.Name()))
	}
	return notes
}
