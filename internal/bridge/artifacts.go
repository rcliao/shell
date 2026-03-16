package bridge

import (
	"log/slog"
	"os"
	"regexp"
	"strings"
)

// artifactRe matches [artifact type="..." path="..." caption="..."] markers from skill scripts.
var artifactRe = regexp.MustCompile(`\[artifact\s+type="([^"]+)"\s+path="([^"]+)"(?:\s+caption="([^"]*)")?\]`)

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
