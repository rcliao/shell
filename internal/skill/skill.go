package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Skill represents a pluggable agent skill loaded from a SKILL.md file.
type Skill struct {
	Name         string   // skill identifier (alphanumeric + hyphens)
	Description  string   // one-line description
	AllowedTools []string // parsed from frontmatter allowed-tools
	Body         string   // full markdown after frontmatter
	Dir          string   // directory containing SKILL.md
	ScriptsDir   string   // path to scripts/ subdirectory (empty if none)
}

// Load reads and parses a SKILL.md file into a Skill.
func Load(skillMdPath string) (*Skill, error) {
	data, err := os.ReadFile(skillMdPath)
	if err != nil {
		return nil, fmt.Errorf("reading skill: %w", err)
	}

	name, desc, allowedTools, body, err := parseFrontmatter(string(data))
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

	return &Skill{
		Name:         name,
		Description:  desc,
		AllowedTools: allowedTools,
		Body:         body,
		Dir:          dir,
		ScriptsDir:   scriptsDir,
	}, nil
}

// LoadDir scans dir/*/SKILL.md and returns all valid skills found.
// Non-existent directories and unparseable skills are silently skipped.
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
		skillMd := filepath.Join(dir, entry.Name(), "SKILL.md")
		s, err := Load(skillMd)
		if err != nil {
			continue // skip invalid skills
		}
		skills = append(skills, s)
	}
	return skills, nil
}

// parseFrontmatter splits SKILL.md content into frontmatter fields and body.
// Frontmatter is delimited by --- lines and contains key: value pairs.
func parseFrontmatter(content string) (name, description string, allowedTools []string, body string, err error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "---") {
		return "", "", nil, "", fmt.Errorf("missing frontmatter delimiter")
	}

	// Find closing ---
	rest := content[3:]
	rest = strings.TrimLeft(rest, "\r\n")
	idx := strings.Index(rest, "---")
	if idx < 0 {
		return "", "", nil, "", fmt.Errorf("missing closing frontmatter delimiter")
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
		return "", "", nil, "", fmt.Errorf("missing required field: name")
	}
	if description == "" {
		return "", "", nil, "", fmt.Errorf("missing required field: description")
	}

	return name, description, allowedTools, body, nil
}
