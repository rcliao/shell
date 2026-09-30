package project

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CheckDocShape refuses an agent write that is not a doc
// (docs/DESIGN-PROJECT-AREAS.md, part 4). Seen live: an agent wrote
// doc-read's JSON back as the doc, and the post summary and every reader
// showed the wrapper for 17 hours. Human edits are never checked.
func CheckDocShape(prev, next string, confirmShrink bool) error {
	t := strings.TrimSpace(next)
	if strings.HasPrefix(t, "{") {
		var env map[string]any
		if json.Unmarshal([]byte(t), &env) == nil {
			if _, ok := env["content"]; ok {
				return fmt.Errorf("this is a JSON envelope, not a doc (doc-read's output?): write the doc's text itself")
			}
		}
		if strings.HasPrefix(t, `{"content"`) {
			return fmt.Errorf("this starts like a JSON envelope, not a doc: write the doc's text itself")
		}
	}
	if docReadHeader.MatchString(t) {
		return fmt.Errorf("this starts with doc-read's header line (rev …): write only the doc's text")
	}
	if !confirmShrink && len(prev) >= 2000 && len(next) < len(prev)/4 {
		return fmt.Errorf("this would remove %d%% of the doc (%d → %d bytes); if that is really intended, write again with --confirm-shrink",
			100-len(next)*100/len(prev), len(prev), len(next))
	}
	return nil
}

var docReadHeader = regexp.MustCompile(`^rev [0-9a-f]{7,40} \(`)
