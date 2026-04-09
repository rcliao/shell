package bridge

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/rcliao/shell/internal/process"
)

// artifactRe matches [artifact type="..." path="..." caption="..."] markers from skill scripts.
var artifactRe = regexp.MustCompile(`\[artifact\s+type="([^"]+)"\s+path="([^"]+)"(?:\s+caption="([^"]*)")?\]`)

// legacyDirectiveRe matches deprecated directive patterns that Claude may still emit.
// Catches both self-closing ([noop]) and block ([relay ...]...[/relay]) forms.
var legacyDirectiveRe = regexp.MustCompile(`(?s)\[(?:relay|schedule|remember|browser|pm|tunnel|heartbeat-learning|task-complete|noop)(?:\s[^\]]*)?\](?:.*?\[/(?:relay|schedule|remember|browser|pm|tunnel|heartbeat-learning|task-complete)\])?`)

// parseArtifacts extracts [artifact type="..." path="..." caption="..."] markers
// from the response, collects image artifacts into photos, and returns the
// cleaned response text.
func (b *Bridge) parseArtifacts(response string, photos *[]Photo) string {
	matches := artifactRe.FindAllStringSubmatchIndex(response, -1)
	if len(matches) == 0 {
		return response
	}

	clean := response
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		artifactType := response[m[2]:m[3]]
		path := response[m[4]:m[5]]
		caption := ""
		if m[6] >= 0 {
			caption = response[m[6]:m[7]]
		}

		switch artifactType {
		case "image":
			data, err := os.ReadFile(path)
			if err != nil {
				slog.Error("artifact: failed to read image", "path", path, "error", err)
				clean = clean[:m[0]] + "(failed to read image)" + clean[m[1]:]
				continue
			}
			*photos = append(*photos, Photo{Data: data, Caption: caption})
			os.Remove(path)
		default:
			slog.Warn("artifact: unknown type", "type", artifactType, "path", path)
		}

		clean = clean[:m[0]] + clean[m[1]:]
	}

	return strings.TrimSpace(clean)
}

// stripDirectives removes any legacy directive markers from the response text.
// Claude is instructed not to emit these, but may still do so occasionally.
func stripDirectives(response string) string {
	cleaned := legacyDirectiveRe.ReplaceAllString(response, "")
	return strings.TrimSpace(cleaned)
}

// summarizeToolCalls produces a short summary when Claude used tools but
// returned no text. This replaces the unhelpful "(empty response)".
func summarizeToolCalls(calls []process.ToolCall) string {
	// Deduplicate tool names, preserving order.
	seen := map[string]int{}
	var names []string
	for _, tc := range calls {
		seen[tc.Name]++
		if seen[tc.Name] == 1 {
			names = append(names, tc.Name)
		}
	}
	var parts []string
	for _, name := range names {
		if seen[name] > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", name, seen[name]))
		} else {
			parts = append(parts, name)
		}
	}
	return fmt.Sprintf("✓ %s", strings.Join(parts, ", "))
}
