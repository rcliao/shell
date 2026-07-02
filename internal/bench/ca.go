package bench

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// ── CA (Convention Adherence) dimension ───────────────────────────────
//
// CA scores whether agents follow shipped conventions over time. Each
// convention is a yaml rule with a regex pattern and a mode
// (must_match / must_not_match). The runner scans recent assistant
// messages per agent, counts violations, and reports compliance.
//
// Goal-state: 1.0 (every shipped convention is honored in every applicable
// message). Drift below 0.95 on any rule is a regression signal.

// CARule is one shipped convention.
type CARule struct {
	ID          string   `yaml:"id"`
	Description string   `yaml:"description"`
	AppliesTo   []string `yaml:"applies_to"` // agent names, empty = all
	Mode        string   `yaml:"mode"`       // must_match | must_not_match
	Pattern     string   `yaml:"pattern"`    // regex, applied to assistant message content
	// MinTokenLen restricts scope: only score messages above this length
	// (useful when a convention only applies to substantive replies).
	MinMsgLen int    `yaml:"min_msg_len,omitempty"`
	ShippedAt string `yaml:"shipped_at,omitempty"`
}

// CAReport is per-agent compliance scoring.
type CAReport struct {
	Agent          string         `json:"agent"`
	MessagesScored int            `json:"messages_scored"`
	Score          float64        `json:"score"` // 1 - (violations / applicable_msgs); avg across rules
	PerRule        []CARuleResult `json:"per_rule"`
}

// CARuleResult is the per-rule outcome.
type CARuleResult struct {
	RuleID     string  `json:"rule_id"`
	Applicable int     `json:"applicable"`
	Violations int     `json:"violations"`
	Compliance float64 `json:"compliance"`
}

// LoadCARules walks a directory of *.yml convention rules.
func LoadCARules(dir string) ([]CARule, error) {
	var rules []CARule
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".yml" && ext != ".yaml" {
			return nil
		}
		buf, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r CARule
		if err := yaml.Unmarshal(buf, &r); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if r.ID == "" {
			r.ID = filepath.Base(path)
		}
		rules = append(rules, r)
		return nil
	})
	return rules, err
}

// ConventionAdherence runs the CA bench for one agent.
func ConventionAdherence(t AgentTarget, rules []CARule, limit int) (*CAReport, error) {
	if limit <= 0 {
		limit = 500
	}
	msgs, err := loadAgentAssistantMessages(t.ShellDB, limit)
	if err != nil {
		return nil, err
	}

	rep := &CAReport{Agent: t.Name, MessagesScored: len(msgs)}
	totalCompliance := 0.0
	scored := 0

	for _, r := range rules {
		if !appliesTo(r, t.Name) {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			rep.PerRule = append(rep.PerRule, CARuleResult{
				RuleID:     r.ID,
				Applicable: 0,
				Violations: 0,
				Compliance: 0,
			})
			continue
		}

		applicable, violations := 0, 0
		for _, m := range msgs {
			if r.MinMsgLen > 0 && len(m) < r.MinMsgLen {
				continue
			}
			applicable++
			matches := re.MatchString(m)
			switch r.Mode {
			case "must_not_match":
				if matches {
					violations++
				}
			case "must_match":
				if !matches {
					violations++
				}
			default:
				// unknown mode → don't score
				applicable--
			}
		}
		var compliance float64 = 1.0
		if applicable > 0 {
			compliance = 1.0 - float64(violations)/float64(applicable)
		}
		rep.PerRule = append(rep.PerRule, CARuleResult{
			RuleID:     r.ID,
			Applicable: applicable,
			Violations: violations,
			Compliance: compliance,
		})
		totalCompliance += compliance
		scored++
	}

	if scored > 0 {
		rep.Score = totalCompliance / float64(scored)
	} else {
		rep.Score = 1.0 // no applicable rules → vacuous compliance
	}
	return rep, nil
}

func appliesTo(r CARule, agent string) bool {
	if len(r.AppliesTo) == 0 {
		return true
	}
	for _, a := range r.AppliesTo {
		if a == agent {
			return true
		}
	}
	return false
}

// Force sql.DB import retention.
var _ = sql.ErrNoRows
