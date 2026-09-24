package store

import (
	"regexp"
	"strings"
	"time"
)

// SkillUse is one skill's real usage, reconstructed from the tool log. The
// retrospective used to read USAGE.jsonl, which only a wrapper the agents
// never use wrote — every skill showed 0 runs and the retro was switched
// off. tool_uses already records every Bash call with its command, so the
// meter was there all along.
type SkillUse struct {
	Runs     int       // script invocations
	Failures int       // of those, tool calls that failed
	Reads    int       // reads of its SKILL.md — a skill read by hand often is one whose rules are not in the prompt
	Last     time.Time // most recent run or read
	Runs7    int       // runs in the last 7 days
}

// skillPathRE finds "/skills/<name>/" followed by scripts/ or SKILL.md,
// optionally inside a version dir (vN/); playground drafts are reported as
// "playground/<name>".
var skillPathRE = regexp.MustCompile(`/skills/(playground/)?([a-z0-9][a-z0-9_-]*)/(?:v[0-9]+/)?(scripts/|SKILL\.md)`)

// SkillUsage returns usage per skill name since a cutoff.
func (s *Store) SkillUsage(since time.Time) (map[string]SkillUse, error) {
	rows, err := s.db.Query(`
		SELECT detail, failed, created_at FROM tool_uses
		WHERE created_at >= ? AND detail LIKE '%/skills/%'`, since.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]SkillUse{}
	week := time.Now().Add(-7 * 24 * time.Hour)
	for rows.Next() {
		var detail string
		var failed bool
		var at time.Time
		if err := rows.Scan(&detail, &failed, &at); err != nil {
			return nil, err
		}
		seen := map[string]bool{} // one command touching a skill twice counts once
		for _, m := range skillPathRE.FindAllStringSubmatch(detail, -1) {
			name := m[1] + m[2]
			kind := "run"
			if m[3] == "SKILL.md" {
				kind = "read"
			}
			if seen[name+kind] {
				continue
			}
			seen[name+kind] = true
			u := out[name]
			if kind == "read" {
				u.Reads++
			} else {
				u.Runs++
				if failed {
					u.Failures++
				}
				if at.After(week) {
					u.Runs7++
				}
			}
			if at.After(u.Last) {
				u.Last = at
			}
			out[name] = u
		}
	}
	return out, rows.Err()
}

// SkillNameFromPath is exported for callers that label a single path.
func SkillNameFromPath(p string) string {
	m := skillPathRE.FindStringSubmatch(p)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1] + m[2])
}
