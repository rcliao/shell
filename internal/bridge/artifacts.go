package bridge

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rcliao/shell/internal/config"
	"github.com/rcliao/shell/internal/process"
	"github.com/rcliao/shell/internal/store"
)

// artifactRe matches [artifact type="..." path="..." caption="..."] markers from skill scripts.
var artifactRe = regexp.MustCompile(`\[artifact\s+type="([^"]+)"\s+path="([^"]+)"(?:\s+caption="([^"]*)")?\]`)

// noopMarkerRe matches a [noop] marker anywhere in the response. Its presence
// means "I chose not to speak" — the whole turn is dropped, so accompanying
// narration ("staying quiet, this is for the other agent") never reaches chat.
var noopMarkerRe = regexp.MustCompile(`(?i)\[noop\]`)

// legacyDirectiveRe matches deprecated directive patterns that Claude may still emit.
// Catches both self-closing ([noop]) and block ([relay ...]...[/relay]) forms.
var legacyDirectiveRe = regexp.MustCompile(`(?s)\[(?:relay|schedule|remember|browser|pm|tunnel|heartbeat-learning|task-complete|noop)(?:\s[^\]]*)?\](?:.*?\[/(?:relay|schedule|remember|browser|pm|tunnel|heartbeat-learning|task-complete)\])?`)

// parseArtifacts extracts [artifact type="..." path="..." caption="..."] markers
// from the response, collects image artifacts into photos and video artifacts
// into videos, and returns the cleaned response text.
func (b *Bridge) parseArtifacts(response string, photos *[]Photo, videos *[]Video) string {
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
			b.archiveArtifact(path)
		case "video":
			data, err := os.ReadFile(path)
			if err != nil {
				slog.Error("artifact: failed to read video", "path", path, "error", err)
				clean = clean[:m[0]] + "(failed to read video)" + clean[m[1]:]
				continue
			}
			*videos = append(*videos, Video{Data: data, Caption: caption})
			b.archiveArtifact(path)
		default:
			slog.Warn("artifact: unknown type", "type", artifactType, "path", path)
		}

		clean = clean[:m[0]] + clean[m[1]:]
	}

	return strings.TrimSpace(clean)
}

// archiveArtifact moves a delivered artifact into the agent's archive dir
// instead of deleting it. Regeneration is nondeterministic and costs real API
// money — deleting originals meant every re-send produced a *different*
// image/video than the one the user approved. Falls back to leaving the file
// in place if the move fails.
func (b *Bridge) archiveArtifact(path string) {
	dir := filepath.Join(config.DefaultConfigDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("artifact archive: mkdir failed, leaving original in place", "error", err)
		return
	}
	// ~/.shell is shared across agent daemons (like worktrees/); prefix with
	// the agent name so archives stay attributable.
	dest := filepath.Join(dir, b.agentBotUsername+"-"+time.Now().Format("20060102-150405")+"-"+filepath.Base(path))
	if err := os.Rename(path, dest); err != nil {
		// Cross-device or permission issue: copy, then best-effort remove.
		data, rerr := os.ReadFile(path)
		if rerr != nil || os.WriteFile(dest, data, 0o644) != nil {
			slog.Warn("artifact archive: move failed, leaving original in place", "path", path, "error", err)
			return
		}
		os.Remove(path)
	}
	slog.Info("artifact archived", "dest", dest)
}

// stripDirectives removes any legacy directive markers from the response text.
// Claude is instructed not to emit these, but may still do so occasionally.
func stripDirectives(response string) string {
	cleaned := legacyDirectiveRe.ReplaceAllString(response, "")
	return strings.TrimSpace(cleaned)
}

// toolUseRows maps observed tool calls to store rows. Detail keeps only a
// short routing-relevant hint (command head, file path, action) — full inputs
// can carry whole message bodies and stay out of the log.
func toolUseRows(calls []process.ToolCall) []store.ToolUse {
	rows := make([]store.ToolUse, 0, len(calls))
	for _, tc := range calls {
		detail := ""
		for _, key := range []string{"command", "file_path", "path", "action", "skill"} {
			if v, ok := tc.Input[key].(string); ok && v != "" {
				detail = head(key+"="+v, 160)
				break
			}
		}
		rows = append(rows, store.ToolUse{Name: tc.Name, Detail: detail, Failed: tc.Failed, DurationMs: tc.DurationMs})
	}
	return rows
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
