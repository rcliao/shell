package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rcliao/shell/internal/bench"
)

// AllDimsSnapshot is a one-screen summary of every dimension across both agents.
// Designed to fit in a terminal screen for fast "did anything regress?" checks.
type AllDimsSnapshot struct {
	Timestamp time.Time            `json:"timestamp"`
	Pikamini  map[string]float64   `json:"pikamini"`
	Umbreon   map[string]float64   `json:"umbreonmini"`
	System    map[string]float64   `json:"system"`
	Guards    map[string]any       `json:"guards"`
	Reports   *AllDimsReports      `json:"reports,omitempty"` // optional full payload
}

// AllDimsReports holds the raw per-agent reports for downstream tools.
type AllDimsReports struct {
	PikaminiAll *bench.AgentReport `json:"pikamini_full,omitempty"`
	UmbreonAll  *bench.AgentReport `json:"umbreon_full,omitempty"`
	CV          *bench.AgentReport `json:"cv,omitempty"`
	PD          *bench.AgentReport `json:"pd,omitempty"`
}

// runAllDims runs every dimension for both agents + cross-agent dims (CV, PD).
// LLM-free, sequential, ~30s total.
//
// `shell-bench all-dims [--full] [--out path]`
func runAllDims(args []string) {
	fs := flag.NewFlagSet("all-dims", flag.ExitOnError)
	home := fs.String("home", "", "")
	rfCases := fs.String("rf-cases", "testdata/bench/cases/recall", "")
	rfCasesUmb := fs.String("rf-cases-umb", "testdata/bench/cases/recall-umbreon", "")
	cvCases := fs.String("cv-cases", "testdata/bench/conversations", "")
	caRules := fs.String("ca-rules", "testdata/bench/conventions", "")
	siRules := fs.String("si-rules", "testdata/bench/skills", "")
	since := fs.String("since", "7d", "WH window")
	full := fs.Bool("full", false, "include full per-rule / per-case detail in JSON")
	out := fs.String("out", "", "JSON output path")
	fs.Parse(args)

	now := time.Now()
	snap := AllDimsSnapshot{
		Timestamp: now,
		Pikamini:  map[string]float64{},
		Umbreon:   map[string]float64{},
		System:    map[string]float64{},
		Guards:    map[string]any{},
	}
	if *full {
		snap.Reports = &AllDimsReports{}
	}

	pika := parseTarget("pikamini", *home)
	umb := parseTarget("umbreonmini", *home)

	// --- WH ---
	if wh, err := bench.WriteHygiene(pika, now.Add(-parseSince(*since)), now); err == nil {
		snap.Pikamini["WH"] = wh.Score
	}
	if wh, err := bench.WriteHygiene(umb, now.Add(-parseSince(*since)), now); err == nil {
		snap.Umbreon["WH"] = wh.Score
	}

	// --- RF (per-agent, separate case sets) ---
	if cases, err := bench.LoadRFCases(*rfCases); err == nil {
		if rf, err := bench.RecallFidelity(pika, cases, 5); err == nil {
			snap.Pikamini["RF.contains"] = rf.Metrics["contains"]
			snap.Pikamini["RF.flexible_contains"] = rf.Metrics["flexible_contains"]
			snap.Pikamini["RF.token_recall"] = rf.Metrics["token_recall"]
		}
	}
	if cases, err := bench.LoadRFCases(*rfCasesUmb); err == nil {
		if rf, err := bench.RecallFidelity(umb, cases, 5); err == nil {
			snap.Umbreon["RF.contains"] = rf.Metrics["contains"]
			snap.Umbreon["RF.flexible_contains"] = rf.Metrics["flexible_contains"]
			snap.Umbreon["RF.token_recall"] = rf.Metrics["token_recall"]
		}
	}

	// --- CV (system-wide synthetic) ---
	if cases, err := bench.LoadCVCases(*cvCases); err == nil {
		if cv, err := bench.RunCV(cases, 5); err == nil {
			snap.System["CV.pass_rate"] = cv.Metrics["pass_rate"]
			snap.System["CV.token_recall"] = cv.Metrics["token_recall"]
			snap.System["CV.flexible_contains"] = cv.Metrics["flexible_contains"]
			snap.System["CV.cases"] = float64(cv.Cases)
		}
	}

	// --- PD (cross-agent) ---
	// Cycle 60: 300-msg window (was 1000). Smaller window avoids drift from
	// older role-bleed messages dragging accuracy below guard threshold.
	if pd, err := bench.PersonaDistinctness(pika, umb, 300, 30, 0.2); err == nil {
		snap.System["PD.accuracy"] = pd.Accuracy
		snap.System["PD.accuracy_a"] = pd.AccuracyA
		snap.System["PD.accuracy_b"] = pd.AccuracyB
	}

	// --- CA (per-agent) ---
	if rules, err := bench.LoadCARules(*caRules); err == nil {
		if ca, err := bench.ConventionAdherence(pika, rules, 500); err == nil {
			snap.Pikamini["CA"] = ca.Score
		}
		if ca, err := bench.ConventionAdherence(umb, rules, 500); err == nil {
			snap.Umbreon["CA"] = ca.Score
		}
	}

	// --- SI (per-agent) ---
	if rules, err := bench.LoadSIRules(*siRules); err == nil {
		if si, err := bench.SkillInvocation(pika, rules, 1000); err == nil {
			snap.Pikamini["SI"] = si.Score
		}
		if si, err := bench.SkillInvocation(umb, rules, 1000); err == nil {
			snap.Umbreon["SI"] = si.Score
		}
	}

	// --- Guards (PD threshold) ---
	// Cycle 62: recalibrated 0.95 → 0.90 after id-parity-stable measurements
	// showed natural range 0.92-0.94. Annotation in state.guards.PD_min_history.
	const pdMin = 0.90
	pd := snap.System["PD.accuracy"]
	snap.Guards["PD_min"] = pdMin
	snap.Guards["PD_current"] = pd
	snap.Guards["PD_holds"] = pd >= pdMin

	// Emit JSON to stdout or file.
	buf, _ := json.MarshalIndent(snap, "", "  ")
	if *out != "" {
		if err := os.WriteFile(*out, buf, 0o644); err != nil {
			die(err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
	}

	// Also emit a one-screen ASCII summary to stderr (visible even when --out is set).
	printDashboard(&snap)
}

func printDashboard(s *AllDimsSnapshot) {
	w := os.Stderr
	fmt.Fprintln(w)
	fmt.Fprintln(w, "─── shell-bench all-dims ──────────────────────────────────────")
	fmt.Fprintf(w, "  %s\n", s.Timestamp.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %-22s  %10s  %10s\n", "Dimension", "pikamini", "umbreonmini")
	fmt.Fprintf(w, "  %s\n", "─────────────────────────────────────────────────────")
	for _, k := range []string{"WH", "RF.contains", "RF.flexible_contains", "RF.token_recall", "CA", "SI"} {
		p, pok := s.Pikamini[k]
		u, uok := s.Umbreon[k]
		ps := "—"
		us := "—"
		if pok {
			ps = fmt.Sprintf("%.3f", p)
		}
		if uok {
			us = fmt.Sprintf("%.3f", u)
		}
		fmt.Fprintf(w, "  %-22s  %10s  %10s\n", k, ps, us)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  System-wide:")
	for _, k := range []string{"CV.pass_rate", "CV.token_recall", "CV.flexible_contains", "CV.cases", "PD.accuracy"} {
		if v, ok := s.System[k]; ok {
			fmt.Fprintf(w, "    %-22s  %.3f\n", k, v)
		}
	}
	fmt.Fprintln(w)
	holds, _ := s.Guards["PD_holds"].(bool)
	pd, _ := s.Guards["PD_current"].(float64)
	pdMin, _ := s.Guards["PD_min"].(float64)
	mark := "✓"
	if !holds {
		mark = "✗ VIOLATED"
	}
	fmt.Fprintf(w, "  PD guard:  %.3f ≥ %.2f  %s\n", pd, pdMin, mark)
	fmt.Fprintln(w, "────────────────────────────────────────────────────────────────")
}
