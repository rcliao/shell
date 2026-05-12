package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Skill tiers control how a skill surfaces in the system prompt.
const (
	TierCore = "core" // full body always inlined (legacy core: true)
	TierHot  = "hot"  // full body inlined subject to per-agent token budget
	TierLazy = "lazy" // one-line catalog entry; body fetched on demand
)

// Skill represents a pluggable agent skill loaded from a SKILL.md file.
type Skill struct {
	Name         string   // skill identifier (alphanumeric + hyphens)
	Description  string   // one-line description
	Usage        string   // one-line usage example for catalog (optional)
	AllowedTools []string // parsed from frontmatter allowed-tools
	Core         bool     // legacy: if true, treated as TierCore
	Tier         string   // "core" | "hot" | "lazy" (default "lazy")
	Version      string   // version folder name (e.g. "v1") or "" for flat layout
	Body         string   // full markdown after frontmatter
	Dir          string   // directory containing the loaded SKILL.md (version dir when versioned)
	SkillRoot    string   // parent dir of the skill (contains ACTIVE and vN/); equals Dir for flat
	ScriptsDir   string   // path to scripts/ subdirectory (empty if none)
}

// Load reads and parses a SKILL.md file into a Skill.
func Load(skillMdPath string) (*Skill, error) {
	data, err := os.ReadFile(skillMdPath)
	if err != nil {
		return nil, fmt.Errorf("reading skill: %w", err)
	}

	name, desc, usage, allowedTools, core, tier, body, err := parseFrontmatter(string(data))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", skillMdPath, err)
	}

	dir := filepath.Dir(skillMdPath)

	// Check for scripts/ subdirectory with at least one executable.
	scriptsDir := ""
	scriptsPath := filepath.Join(dir, "scripts")
	if info, err := os.Stat(scriptsPath); err == nil && info.IsDir() {
		entries, _ := os.ReadDir(scriptsPath)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			fi, err := e.Info()
			if err == nil && fi.Mode()&0111 != 0 {
				scriptsDir = scriptsPath
				break
			}
		}
	}

	// Normalize tier. Legacy `core: true` wins over `tier:`. Default is lazy.
	resolvedTier := tier
	if core {
		resolvedTier = TierCore
	}
	if resolvedTier == "" {
		resolvedTier = TierLazy
	}

	return &Skill{
		Name:         name,
		Description:  desc,
		Usage:        usage,
		AllowedTools: allowedTools,
		Core:         core,
		Tier:         resolvedTier,
		Body:         body,
		Dir:          dir,
		SkillRoot:    dir,
		ScriptsDir:   scriptsDir,
	}, nil
}

// loadVersioned reads skillRoot/ACTIVE, resolves to skillRoot/<version>/SKILL.md,
// and loads that version. Returns ErrNotVersioned if ACTIVE is absent.
func loadVersioned(skillRoot string) (*Skill, error) {
	activePath := filepath.Join(skillRoot, "ACTIVE")
	data, err := os.ReadFile(activePath)
	if err != nil {
		return nil, err
	}
	version := strings.TrimSpace(string(data))
	if version == "" {
		return nil, fmt.Errorf("ACTIVE file empty: %s", activePath)
	}
	// Guard against path traversal — version must be a single path segment.
	if strings.ContainsAny(version, "/\\") || version == "." || version == ".." {
		return nil, fmt.Errorf("ACTIVE contains invalid version %q: %s", version, activePath)
	}
	versionDir := filepath.Join(skillRoot, version)
	info, err := os.Stat(versionDir)
	if err != nil {
		return nil, fmt.Errorf("version dir missing: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("version path is not a directory: %s", versionDir)
	}
	s, err := Load(filepath.Join(versionDir, "SKILL.md"))
	if err != nil {
		return nil, err
	}
	s.Version = version
	s.SkillRoot = skillRoot
	return s, nil
}

// LoadDir scans dir/*/SKILL.md and returns all valid skills found.
// Non-existent directories and unparseable skills are silently skipped.
//
// Supports two layouts:
//
//	flat:       dir/<skill-name>/SKILL.md
//	versioned:  dir/<skill-name>/ACTIVE (contents: "v1") + dir/<skill-name>/v1/SKILL.md
//
// When both are present, versioned wins (ACTIVE takes precedence).
// The "playground" and ".archive" directories are reserved and always skipped.
func LoadDir(dir string) ([]*Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading skills dir: %w", err)
	}

	var skills []*Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "playground" || name == ".archive" || strings.HasPrefix(name, ".") {
			continue
		}
		skillRoot := filepath.Join(dir, name)

		// Prefer versioned layout if ACTIVE is present.
		if _, err := os.Stat(filepath.Join(skillRoot, "ACTIVE")); err == nil {
			s, err := loadVersioned(skillRoot)
			if err != nil {
				continue
			}
			skills = append(skills, s)
			continue
		}

		// Fall back to flat layout.
		s, err := Load(filepath.Join(skillRoot, "SKILL.md"))
		if err != nil {
			continue
		}
		skills = append(skills, s)
	}
	return skills, nil
}

// parseFrontmatter splits SKILL.md content into frontmatter fields and body.
// Frontmatter is delimited by --- lines and contains key: value pairs.
func parseFrontmatter(content string) (name, description, usage string, allowedTools []string, core bool, tier, body string, err error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return "", "", "", nil, false, "", "", fmt.Errorf("missing frontmatter delimiter")
	}

	// Find closing ---
	rest := content[3:]
	rest = strings.TrimLeft(rest, "\r\n")
	idx := strings.Index(rest, "---")
	if idx < 0 {
		return "", "", "", nil, false, "", "", fmt.Errorf("missing closing frontmatter delimiter")
	}

	frontmatter := rest[:idx]
	body = strings.TrimSpace(rest[idx+3:])

	// Parse flat key: value lines
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		switch key {
		case "name":
			name = value
		case "description":
			description = value
		case "usage":
			usage = value
		case "core":
			core = value == "true"
		case "tier":
			switch strings.ToLower(value) {
			case TierCore, TierHot, TierLazy:
				tier = strings.ToLower(value)
			}
		case "allowed-tools":
			// Spec uses space-delimited; also support comma-delimited for compat.
			sep := " "
			if strings.Contains(value, ",") {
				sep = ","
			}
			for _, t := range strings.Split(value, sep) {
				t = strings.TrimSpace(t)
				if t != "" {
					allowedTools = append(allowedTools, t)
				}
			}
		}
	}

	if name == "" {
		return "", "", "", nil, false, "", "", fmt.Errorf("missing required field: name")
	}
	if description == "" {
		return "", "", "", nil, false, "", "", fmt.Errorf("missing required field: description")
	}

	return name, description, usage, allowedTools, core, tier, body, nil
}
