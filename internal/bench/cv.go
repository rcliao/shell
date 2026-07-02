package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	memory "github.com/rcliao/ghost"
	"gopkg.in/yaml.v3"
)

// ── Conversation-seed eval (CV dimension) ─────────────────────────────
//
// CV measures the memory infrastructure's ability to answer probes after
// a multi-turn conversation has been processed. It does NOT run the live
// agent — instead it injects `expected_memory_state` rows into a fresh
// sandbox ghost DB, runs probes via FTS5, and scores with the same
// LLM-free primitives as RF. The goal is to vary conversation shape
// (locales, date forms, cross-session) and find infrastructure failures
// faster than waiting for real user complaints.

// SeedTurn is one turn in a conversation seed.
type SeedTurn struct {
	Turn    int    `yaml:"turn"`
	Speaker string `yaml:"speaker"`
	Content string `yaml:"content"`
}

// SeedMemory is one row injected into the sandbox before probes run.
type SeedMemory struct {
	Key     string   `yaml:"key"`
	Tier    string   `yaml:"tier"`
	Pinned  bool     `yaml:"pinned"`
	Tags    []string `yaml:"tags"`
	Content string   `yaml:"content"`
}

// Probe is a single CV question with token-recall expectations.
type Probe struct {
	ID                  string   `yaml:"id"`
	Question            string   `yaml:"question"`
	ExpectedTokens      []string `yaml:"expected_tokens"`
	MinTokenRecall      float64  `yaml:"min_token_recall"`
	MinFlexibleContains float64  `yaml:"min_flexible_contains"`
	Tags                []string `yaml:"tags"`
}

// CVCase is one conversation seed parsed from YAML.
type CVCase struct {
	ID                   string       `yaml:"id"`
	Description          string       `yaml:"description"`
	PersonaArchetype     string       `yaml:"persona_archetype"`
	AgentNS              string       `yaml:"agent_ns"`
	Seed                 []SeedTurn   `yaml:"seed"`
	ExpectedMemoryState  []SeedMemory `yaml:"expected_memory_state"`
	Probes               []Probe      `yaml:"probes"`
}

// CVReport — aggregate scores across all CV cases.
type CVReport struct {
	Cases   int                `json:"cases"`
	Probes  int                `json:"probes"`
	Metrics map[string]float64 `json:"metrics"` // averaged over probes
	PerCase []CVCaseResult     `json:"per_case,omitempty"`
}

// CVCaseResult — outcome for a single conversation seed.
type CVCaseResult struct {
	CaseID  string          `json:"case_id"`
	Probes  []CVProbeResult `json:"probes"`
	Passed  int             `json:"passed"`
	Failed  int             `json:"failed"`
}

// CVProbeResult — outcome for a single probe.
type CVProbeResult struct {
	ProbeID           string             `json:"probe_id"`
	Question          string             `json:"question"`
	Metrics           map[string]float64 `json:"metrics"`
	MinTokenRecall    float64            `json:"min_token_recall"`
	MinFlexible       float64            `json:"min_flexible_contains"`
	Passed            bool               `json:"passed"`
	RetrievedKeys     []string           `json:"retrieved_keys"`
	BestSnippet       string             `json:"best_snippet,omitempty"`
}

// LoadCVCases loads all *.yml conversation seeds from dir.
func LoadCVCases(dir string) ([]CVCase, error) {
	var cases []CVCase
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
		var c CVCase
		if err := yaml.Unmarshal(buf, &c); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if c.ID == "" {
			c.ID = filepath.Base(path)
		}
		cases = append(cases, c)
		return nil
	})
	return cases, err
}

// RunCV evaluates every conversation seed against a fresh sandbox ghost DB.
//
// For each case:
//  1. Create a temp SQLite file
//  2. Open a ghost store backed by it
//  3. Put every expected_memory_state row into the case's agent_ns
//  4. For each probe: FTS5 search, score with LLM-free metrics, pass/fail
//     against the probe's min_* thresholds
//  5. Discard the temp DB
//
// Aggregate metrics + pass/fail land in the report. No live state touched.
func RunCV(cases []CVCase, topK int) (*CVReport, error) {
	return RunCVWithNoise(cases, topK, 0)
}

// RunCVWithNoise extends RunCV by injecting `noiseN` unrelated memories
// into each sandbox before running probes. This simulates the ranker
// competition agents face on real production DBs (1000s of memories,
// where target content must beat noise to surface in top-K).
//
// Cycle 64: added so CV can faithfully reproduce the owner's "memory mixed
// across topics" failure shape that doesn't manifest in 4-row sandboxes.
func RunCVWithNoise(cases []CVCase, topK int, noiseN int) (*CVReport, error) {
	if topK <= 0 {
		topK = 5
	}
	rep := &CVReport{
		Metrics: map[string]float64{
			"token_recall":      0,
			"flexible_contains": 0,
			"pass_rate":         0,
		},
	}
	probeCount := 0
	totalPassed := 0

	for _, c := range cases {
		caseRes := CVCaseResult{CaseID: c.ID}

		tmp, err := os.CreateTemp("", "shell-bench-cv-*.db")
		if err != nil {
			return nil, fmt.Errorf("temp file: %w", err)
		}
		tmpPath := tmp.Name()
		tmp.Close()

		func() {
			defer os.Remove(tmpPath)
			defer os.Remove(tmpPath + "-shm")
			defer os.Remove(tmpPath + "-wal")

			store, err := memory.NewSQLiteStore(tmpPath)
			if err != nil {
				caseRes.Failed = len(c.Probes)
				rep.PerCase = append(rep.PerCase, caseRes)
				return
			}

			ctx := context.Background()
			ns := c.AgentNS
			if ns == "" {
				ns = "agent:cv-sandbox"
			}
			for _, sm := range c.ExpectedMemoryState {
				tier := sm.Tier
				if tier == "" {
					tier = "ltm"
				}
				_, _ = store.Put(ctx, memory.PutParams{
					NS:      ns,
					Key:     sm.Key,
					Content: sm.Content,
					Kind:    "semantic",
					Tags:    sm.Tags,
					Tier:    tier,
					Pinned:  sm.Pinned,
				})
			}

			// Cycle 64: inject N unrelated memories so target content has
			// to compete in ranking. Simulates real-DB noise floor.
			if noiseN > 0 {
				for i := 0; i < noiseN; i++ {
					_, _ = store.Put(ctx, memory.PutParams{
						NS:      ns,
						Key:     fmt.Sprintf("noise-%04d", i),
						Content: cvNoiseSample(i),
						Kind:    "episodic",
						Tags:    []string{"noise", "synthetic"},
						Tier:    "stm",
					})
				}
			}

			for _, p := range c.Probes {
				retrieved, snippet := ghostSearch(ctx, store, ns, p.Question, topK)
				gold := joinTokens(p.ExpectedTokens)
				metrics := map[string]float64{
					"token_recall":      TokenRecall(snippet, gold),
					"flexible_contains": FlexibleContains(snippet, gold),
					"contains":          Contains(snippet, gold),
				}
				passed := metrics["token_recall"] >= p.MinTokenRecall &&
					metrics["flexible_contains"] >= p.MinFlexibleContains

				caseRes.Probes = append(caseRes.Probes, CVProbeResult{
					ProbeID:        p.ID,
					Question:       p.Question,
					Metrics:        metrics,
					MinTokenRecall: p.MinTokenRecall,
					MinFlexible:    p.MinFlexibleContains,
					Passed:         passed,
					RetrievedKeys:  retrieved,
					BestSnippet:    truncate(snippet, 200),
				})
				if passed {
					caseRes.Passed++
					totalPassed++
				} else {
					caseRes.Failed++
				}
				probeCount++
				rep.Metrics["token_recall"] += metrics["token_recall"]
				rep.Metrics["flexible_contains"] += metrics["flexible_contains"]
			}
		}()

		rep.PerCase = append(rep.PerCase, caseRes)
	}

	rep.Cases = len(cases)
	rep.Probes = probeCount
	if probeCount > 0 {
		for k := range rep.Metrics {
			if k != "pass_rate" {
				rep.Metrics[k] /= float64(probeCount)
			}
		}
		rep.Metrics["pass_rate"] = float64(totalPassed) / float64(probeCount)
	}
	return rep, nil
}

// cvNoiseSample returns a realistic-looking unrelated memory body for
// noise injection. Cycles through ~25 distinct templates (meal-memos,
// fortune readings, heartbeats, family chat) so retrieval has to fight
// real competition. Deterministic per index.
func cvNoiseSample(i int) string {
	templates := []string{
		"User: 早餐memo - toast, latte\nAssistant: saved 📝 breakfast logged",
		"User: 晚餐memo - quinoa + steak\nAssistant: 📝 dinner saved, dairy 0pt",
		"User: snack memo - tj's pretzels\nAssistant: noted, snack logged",
		"User: [Heartbeat] check activity\nAssistant: quiet morning, nothing to report",
		"User: 今天天氣怎樣\nAssistant: 76°F sunny, light breeze",
		"User: any fortune for today?\nAssistant: 🌙 saturday's energy: quiet, observant",
		"User: owner schedule for tomorrow\nAssistant: clear morning, 2pm meeting",
		"User: did we get groceries\nAssistant: yes, costco run Saturday",
		"User: 提醒我 8pm flonase\nAssistant: scheduled, reminder set for 8pm",
		"User: how was the walk\nAssistant: ✓ logged, felt good per owner",
		"User: family group photo this weekend\nAssistant: noted, planning for sunday brunch",
		"User: book club next thursday\nAssistant: added to calendar",
		"User: kid's soccer practice change\nAssistant: noted, new time Wed 5pm",
		"User: water bill due\nAssistant: due 5/20, $89, scheduled reminder",
		"User: dentist appointment confirmed\nAssistant: ✓ 5/25 10am, marked",
		"User: dry cleaning ready\nAssistant: pickup window through Saturday",
		"User: jellycat order arrived\nAssistant: 🧸 confirmed receipt logged",
		"User: car service due\nAssistant: due in 500mi, scheduled",
		"User: coffee shop tip\nAssistant: noted tasting notes",
		"User: weekend trip planning\nAssistant: hotel options noted",
		"User: birthday gift ideas\nAssistant: 3 candidates logged",
		"User: weather forecast weekend\nAssistant: sunny saturday, rain sunday",
		"User: dog walker availability\nAssistant: available wed/fri",
		"User: home repair quote\nAssistant: $450 estimate logged",
		"User: garage sale prep\nAssistant: items inventoried",
	}
	return templates[i%len(templates)]
}

func joinTokens(ts []string) string {
	out := ""
	for i, t := range ts {
		if i > 0 {
			out += " "
		}
		out += t
	}
	return out
}
