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

// ── SI (Skill Invocation) dimension ──────────────────────────────────────
//
// SI scores whether an agent invokes the right skill on natural trigger
// phrases. For each user message that matches a trigger regex, we check
// whether the immediately-following assistant message contains the
// expected skill-output signature (file paths for image gen, "scheduled"
// for shell-schedule, "saved" for shell-remember, etc).
//
// LLM-free: pure regex matching against shell.db user→assistant pairs.
// Score = correct invocations / total trigger matches, averaged per rule.
//
// Goal-state interpretation:
//   SI ≥ 0.85 → agent reliably reaches for the right tool
//   SI ≈ 0.5  → coin flip; tool routing broken
//   SI < 0.5  → agent ignoring or mis-routing triggers

// SIRule is one trigger → expected-output pair.
type SIRule struct {
	ID                 string `yaml:"id"`
	Description        string `yaml:"description"`
	Skill              string `yaml:"skill"`
	TriggerPattern     string `yaml:"trigger_pattern"`      // regex on user message
	OutputPattern      string `yaml:"output_pattern"`       // regex on assistant reply (success signature)
	NegativeOutputHint string `yaml:"negative_output_hint"` // optional: refusal/failure markers
	// MaxUserLen filters out long user messages (likely meta-discussions or
	// digressions that mention the skill keyword without actually requesting
	// the skill). 0 = no limit.
	MaxUserLen int `yaml:"max_user_len,omitempty"`
	// MinUserLen filters out trivial 1-2 char triggers that are unlikely to
	// be real requests. 0 = no limit.
	MinUserLen int `yaml:"min_user_len,omitempty"`
}

// SIReport holds per-agent SI scoring.
type SIReport struct {
	Agent        string         `json:"agent"`
	PairsScanned int            `json:"pairs_scanned"`
	Score        float64        `json:"score"` // avg compliance across applicable rules
	PerRule      []SIRuleResult `json:"per_rule"`
}

// SIRuleResult per-rule outcome.
type SIRuleResult struct {
	RuleID    string  `json:"rule_id"`
	Triggers  int     `json:"triggers"`
	Correct   int     `json:"correct"`
	Refusals  int     `json:"refusals"`
	Accuracy  float64 `json:"accuracy"`
	Examples  []SIPair `json:"examples,omitempty"` // up to 3 failures, truncated
}

// SIPair is a sample user/assistant exchange for diagnostic display.
type SIPair struct {
	UserMsgID      int64  `json:"user_msg_id"`
	UserExcerpt    string `json:"user_excerpt"`
	AssistExcerpt  string `json:"assist_excerpt"`
	MatchedTrigger bool   `json:"matched_trigger"`
	MatchedOutput  bool   `json:"matched_output"`
}

// LoadSIRules walks dir, parses every *.yml as an SIRule.
func LoadSIRules(dir string) ([]SIRule, error) {
	var rules []SIRule
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
		var r SIRule
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

// SkillInvocation walks recent user→assistant pairs and scores each rule.
func SkillInvocation(t AgentTarget, rules []SIRule, limit int) (*SIReport, error) {
	if limit <= 0 {
		limit = 1000
	}
	db, err := sql.Open("sqlite", t.ShellDB+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	// Pull user messages; for each, gather up to 3 next assistant messages
	// in the same session. Multi-turn lookback so deferred-image style
	// responses (think-first, generate-next-turn) don't read as failures.
	rows, err := db.Query(`
		SELECT id, content, session_id
		FROM messages
		WHERE role = 'user'
		ORDER BY id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type pair struct {
		uid      int64
		uContent string
		aContent string // concatenated next-3 assistant replies in same session
	}
	type rawUser struct {
		uid       int64
		uContent  string
		sessionID int64
	}
	var users []rawUser
	for rows.Next() {
		var ru rawUser
		if err := rows.Scan(&ru.uid, &ru.uContent, &ru.sessionID); err != nil {
			return nil, err
		}
		users = append(users, ru)
	}
	rows.Close()

	// Pre-fetch next-3 assistant replies per user msg (concat with separator).
	pairs := make([]pair, 0, len(users))
	for _, ru := range users {
		nx, err := db.Query(`
			SELECT content FROM messages
			WHERE id > ? AND session_id = ? AND role='assistant'
			ORDER BY id ASC LIMIT 3`, ru.uid, ru.sessionID)
		if err != nil {
			pairs = append(pairs, pair{uid: ru.uid, uContent: ru.uContent})
			continue
		}
		var buf string
		for nx.Next() {
			var c string
			nx.Scan(&c)
			buf += c + "\n---\n"
		}
		nx.Close()
		pairs = append(pairs, pair{uid: ru.uid, uContent: ru.uContent, aContent: buf})
	}

	rep := &SIReport{Agent: t.Name, PairsScanned: len(pairs)}
	totalAccuracy := 0.0
	scored := 0

	// Refusal heuristic: keep simple, catches generic decline patterns.
	refusalRe := regexp.MustCompile(`(?i)(can't|cannot|抱歉|不行|無法|sorry,? I)`)

	for _, r := range rules {
		trigRe, err := regexp.Compile(r.TriggerPattern)
		if err != nil {
			continue
		}
		outRe, err := regexp.Compile(r.OutputPattern)
		if err != nil {
			continue
		}

		triggers, correct, refusals := 0, 0, 0
		var examples []SIPair

		for _, p := range pairs {
			if r.MaxUserLen > 0 && len(p.uContent) > r.MaxUserLen {
				continue
			}
			if r.MinUserLen > 0 && len(p.uContent) < r.MinUserLen {
				continue
			}
			if !trigRe.MatchString(p.uContent) {
				continue
			}
			triggers++
			matched := outRe.MatchString(p.aContent)
			if matched {
				correct++
			} else if refusalRe.MatchString(p.aContent) {
				refusals++
			}
			if !matched && len(examples) < 3 {
				examples = append(examples, SIPair{
					UserMsgID:     p.uid,
					UserExcerpt:   truncate(p.uContent, 80),
					AssistExcerpt: truncate(p.aContent, 80),
					MatchedTrigger: true,
					MatchedOutput:  false,
				})
			}
		}

		var accuracy float64 = 1.0
		if triggers > 0 {
			accuracy = float64(correct) / float64(triggers)
		}
		rep.PerRule = append(rep.PerRule, SIRuleResult{
			RuleID:   r.ID,
			Triggers: triggers,
			Correct:  correct,
			Refusals: refusals,
			Accuracy: accuracy,
			Examples: examples,
		})
		if triggers > 0 {
			totalAccuracy += accuracy
			scored++
		}
	}

	if scored > 0 {
		rep.Score = totalAccuracy / float64(scored)
	} else {
		rep.Score = 0
	}
	return rep, nil
}
