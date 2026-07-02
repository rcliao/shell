package bench

import "time"

// AgentTarget identifies one agent's data sources for a bench run.
type AgentTarget struct {
	Name       string // e.g. "pikamini"
	ShellDB    string // ~/.shell/agents/<name>/shell.db
	MemoryDB   string // ~/.shell/agents/<name>/memory.db (ghost)
	Namespace  string // expected ghost namespace, e.g. "agent:pikamini"
	OwnerChats []int64 // chat IDs whose user messages count as "real" (the owners)
}

// Score is a per-metric numeric result with a count of observations.
type Score struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	Count  int     `json:"count"`
}

// AgentReport is the output for a single agent across one or more dimensions.
type AgentReport struct {
	Agent     string    `json:"agent"`
	Timestamp time.Time `json:"timestamp"`
	WH        *WHReport `json:"write_hygiene,omitempty"`
	RF        *RFReport `json:"recall_fidelity,omitempty"`
	CV        *CVReport `json:"conversation_eval,omitempty"`
	PD        *PDReport `json:"persona_distinctness,omitempty"`
	CA        *CAReport `json:"convention_adherence,omitempty"`
	SI        *SIReport `json:"skill_invocation,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
}

// WHReport — Write Hygiene results for one agent over a time window.
type WHReport struct {
	WindowStart    time.Time     `json:"window_start"`
	WindowEnd      time.Time     `json:"window_end"`
	ClaimedWrites  int           `json:"claimed_writes"`
	VerifiedWrites int           `json:"verified_writes"`
	WrongNamespace int           `json:"wrong_namespace"`
	MissingRow     int           `json:"missing_row"`
	Score          float64       `json:"score"` // verified / claimed
	Samples        []WHViolation `json:"samples,omitempty"`
}

// WHViolation — one claim that failed verification (truncated for readability).
type WHViolation struct {
	MessageID int64     `json:"message_id"`
	ChatID    int64     `json:"chat_id"`
	When      time.Time `json:"when"`
	Reason    string    `json:"reason"`
	Excerpt   string    `json:"excerpt"`
}

// RFReport — Recall Fidelity results across all loaded cases.
type RFReport struct {
	Cases      int                `json:"cases"`
	Metrics    map[string]float64 `json:"metrics"` // averaged across cases
	PerCase    []RFCaseResult     `json:"per_case,omitempty"`
}

// RFCaseResult — outcome for a single Q-A case.
type RFCaseResult struct {
	CaseID     string             `json:"case_id"`
	Question   string             `json:"question"`
	Retrieved  []string           `json:"retrieved_keys"`
	Metrics    map[string]float64 `json:"metrics"`
	BestSnippet string            `json:"best_snippet,omitempty"`
}
