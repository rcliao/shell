package skill

import (
	"fmt"
	"strings"
)

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

// CatalogPrompt returns a compact one-liner-per-skill listing for the system prompt.
// Saves tokens compared to SystemPrompt() by omitting full skill bodies.
func (r *Registry) CatalogPrompt() string {
	if len(r.skills) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n\n## Available Skills\n\n")
	sb.WriteString("**IMPORTANT: Artifact markers.** When a skill script outputs lines matching `[artifact type=\"...\" path=\"...\" caption=\"...\"]`, ")
	sb.WriteString("you MUST include them verbatim in your response text. The bridge parses these markers to deliver binary content (images, files) to the user. ")
	sb.WriteString("Do not omit, paraphrase, or summarize artifact markers.\n\n")
	sb.WriteString("To load full instructions for a skill, run: `scripts/shell-skill load <name>`\n\n")
	for _, s := range r.skills {
		sb.WriteString("- **")
		sb.WriteString(s.Name)
		sb.WriteString("**: ")
		sb.WriteString(s.Description)
		if s.ScriptsDir != "" {
			sb.WriteString(" (`")
			sb.WriteString(s.ScriptsDir)
			sb.WriteString("/`)")
		}
		sb.WriteString("\n")
	}
	return sb.String()
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
