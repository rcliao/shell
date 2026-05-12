# Backlog

**Status flow** (per README):
- Low-risk path: `proposed` → loop ships → `validating` → `shipped` | `regressed`
- High-risk path: `proposed` → mami/papi flips to `approved` → loop ships → `validating` → `shipped`
- Other terminals: `rejected`, `superseded`

Items grouped by status. Within each, ordered by ROI / unblocking value.

---

## 🟢 Active (proposed, awaiting ship)

### B-021 — Stop polluting `agent:pikamini` ns with dev-session summaries  *(HIGH-RISK → needs approval)*
- **status:** proposed
- **kind:** investigation + code change in Claude Code session-summary hook + data migration
- **why:** 730 `session-*` memories in `agent:pikamini` ns. Sample shows summaries from Claude Code dev sessions about shell/ghost (e.g. `session-shell-2026-05-07-1d3965fe`, `session-pikamini-2026-05-07-1d3965fe`) — these are MY dev-environment session summaries, not pikamini-the-agent's sessions. The auto-memory hook routes by cwd → ns; cwd `~/src/pikamini/projects/shell` becomes `agent:pikamini` because the project root is named "pikamini". This contaminates the agent's identity space and inflates retrieval noise.
- **scope:**
  1. Find the hook that writes these (likely Claude Code's session-end hook or `auto-memory` skill).
  2. Reroute future writes to a separate ns (e.g. `dev:shell`, `dev:ghost`, or `agent:claude-code`).
  3. Migrate the 730 existing rows: re-namespace them based on content/context (those mentioning shell/ghost/MCP code go to `dev:*`; pikamini's actual session summaries stay).
  4. Better: tag-based filter — keep them in `agent:pikamini` but with `dev-session` tag and a reflect rule to suppress them in agent context retrieval.
- **risk:** mass mutation across 730 rows; affects retrieval scoring; could lose pikamini's real session summaries if migration mis-classifies.
- **predicted-effect:** pikamini's context retrieval becomes ~30% leaner; identity coherence improves; my dev sessions stop drifting into agent's self-concept.

### B-019 — Mass-tag existing auto-summary memories with `auto-generated`  *(HIGH-RISK → needs approval)*
- **status:** proposed
- **kind:** mass UPDATE adding tag (~900 rows: 347 pikamini + 562 umbreon)
- **why:** B-013a rule is live but fires on 0 rows because no memory carries the `auto-generated` tag. Tagging existing rows lets the rule archive accumulated dormant clutter.
- **scope:** `UPDATE memories SET tags = json_insert(coalesce(tags,'[]'), '$[#]', 'auto-generated') WHERE (key LIKE 'auto-summary-%' OR key LIKE 'exchange-summary-%') AND coalesce(tags,'') NOT LIKE '%auto-generated%';`
- **risk:** mass mutation but additive (only tags); reversible. Backup both DBs first.
- **predicted-effect:** dormant count drops ≥30% per agent within 1 reflect cycle.

### B-013 — Reflect rule to prune stale auto-summary memories  *(SHIPPED partial → see B-013a / B-019)*
- **status:** shipped (cycle 21, rule only — see B-013a; mass tagging gated as B-019)
- **kind:** ghost reflect rule (SQL insert per agent)
- **why:** `auto-summary-*` and `exchange-summary-*` keys accumulate in dormant tier with utility_count=0, dominating top-access lists and obscuring real signal.
- **scope:** insert reflect_rule per agent: `cond_tag_includes='auto-summary'` (or key glob match in app code if SQL alone can't do it), tier='dormant', age>30d → action=ARCHIVE. After another 30d → DELETE.
- **risk:** low; ARCHIVE is reversible.
- **predicted-effect:** dormant count drops by ≥30% within 1 reflect cycle on each agent.
- **measure-by:** 24h after ship.

### B-016 — Add ghost vs MEMORY.md decision-tree convention  *(SHIPPED cycle 21)*
- **status:** validating (predicted-effect: agents reference convention next storage-related convo; measure-by: opportunistic)
- pikamini ghost id `01KR3WS0X8Y9WW7Z62CG0M0DKM` · umbreon ghost id `01KR3WS3WHGMB2518EDARRTTS5`
- **kind:** behavior rule (ghost put both agents, tag `convention`)
- **why:** Cycle 6 caught both agents uncertain. Plant records initially missed ghost. One clear rule both agents can recall closes the loop.
- **scope:** ghost put pinned-ltm in both ns: storage decision tree (mami facts → ghost; agent conventions → ghost; papi dev notes → MEMORY.md; when uncertain, prefer ghost).
- **risk:** none.
- **predicted-effect:** observable in next mami DM about storage; agents reference the convention.
- **measure-by:** opportunistic (next storage-related conversation).

### B-001 — Implement skill USAGE.jsonl logging  *(low-risk; deferred)*
- **kind:** code-fix (shell repo)
- **why:** Originally P0 but cycle-4 method (`shell.db` empty-row query) gave us the noop telemetry we actually needed. Real skill-success-rate would still benefit from this once we have several skills in flight.
- **scope:** `scripts/run-skill` wrapper logs `{ts, skill, agent, exit_code, duration_ms}`. Rotate at 10 MB.
- **risk:** low.
- **gating:** blocked by shell repo WIP (54 modified files). Loop won't touch bridge until WIP commits.

### B-008 — Remove legacy `[relay to=...]` text-directive parser  *(low-risk; deferred)*
- **kind:** code-fix (shell repo)
- **why:** 0 invocations in 1.5 months. Dead code in `internal/bridge/artifacts.go` regex.
- **gating:** blocked by shell repo WIP. Will pick up once WIP commits.

### B-017 — Resolve heartbeat prompt internal tension  *(low-risk; deferred)*
- **kind:** prompt-only edit (shell repo)
- **why:** Lines 203/206 of heartbeat.go conflict — "send a check-in" vs "[noop] if nothing." Drives umbreon's 27% noop rate vs pikamini's 55%.
- **scope:** gate the check-in instruction on having something specific to share.
- **risk:** low; reversible single string change.
- **gating:** blocked by shell repo WIP. Loop won't edit bridge until WIP commits.
- **predicted-effect:** umbreon heartbeat noop rate ↑ from 27% toward 40%+ within 24h after ship.
- **measure-by:** 24h after ship; baseline captured in cycle 8.

### B-014 — Prune weak edges in pikamini ghost graph  *(HIGH-RISK → needs approval)*
- **kind:** mass DELETE on memory_edges (tens of thousands of rows)
- **why:** pikamini avg edge weight 0.22 vs umbreon 0.59. Many weak edges → noisy expansion.
- **scope:** baseline `ghost eval --simulate`, then either `DELETE FROM memory_edges WHERE weight < 0.15` for pikamini ns (mass mutation) OR test `GHOST_EDGE_MIN_WEIGHT=0.3` env var first.
- **risk:** mass deletion of working data — needs explicit approval. Backup first.

### B-003 — Migrate pikamini transcripts off shared DB  *(HIGH-RISK → needs approval)*
- **kind:** data migration touching 1847+ rows
- **why:** New design rule: each agent owns its transcript. Pikamini still writes to `~/.shell/shared/transcript-v2.db`.
- **scope:** add `transcript.db_path` to pikamini config, copy/move historical rows, switch writer.
- **risk:** must not lose history. Mass row movement.

### B-004 — Spec the unified Task Brain ledger  *(low-risk; speculative)*
- **kind:** docs (shell repo)
- **why:** OpenClaw inspiration. Useful long-term but no immediate pain point.
- **gating:** unblocks once any of the four ledgers (tasks.db, scheduler, shell-pm, heartbeat reflections) has an actual conflict; today they coexist.

---

## 👁 Observing (gather more samples before codifying)

### B-015 — Codify "cross-agent fact-check" convention
- One sample observed (cycle 6). Need 2–3 more before writing the rule. Loop checks each cycle.

---

## ✅ Shipped

- **B-020** — telegram-bullets-not-tables convention (cycle 23, repeat correction from mami). pikamini `01KR43ZQAX9Y9JEZ7YM4K1AAY6`, umbreon `01KR43ZVYR0YH77RWN4TGYYN9R`.
  - **GRADED shipped-validated (cycle 30, 24h check):** 91 agent messages in window, 0 markdown-table violations. 1 ASCII-art diagram with pipes detected but is appropriate (plant-cutting guide). Convention is being respected. Will continue passive watch through cycle 36 (full 7-day window).
- **B-005** — verify-after-schedule convention (cycle 3, ghost id `01KR1Q3K2PNTDCJTHSHGEB1KFQ`)
- **B-006** — umbreon reflect rules harmonized (cycle 16, SQL backup at `memory.db.bak-2026-05-07-c16`)
  - **GRADED regressed-or-wrong-lever (cycle 28, 30h post-ship):**
    - Umbreon hb noop 29.1% → 28.9% → **27.0%** (-2.1pp wrong direction)
    - Pikamini control 55.4% → 51.4% → **48.1%** (-7.3pp same direction)
    - Both agents trended DOWN. Drift is ambient, not umbreon-specific. Reflect rules are NOT the lever.
    - Conclusion: B-017 (heartbeat prompt tension fix) is the next experiment. Still blocked by shell bridge WIP.
    - **No revert needed** — the rules harmonization is still a correctness win (umbreon now has the same identity-protection as pikamini). It just doesn't move the noop metric. Side benefits remain: identity tier protected from decay, faster ltm promotion. Mark `shipped-but-no-metric-effect` rather than `regressed`.
- **B-007** — `meal-memo` + `gentle-checkin` skill files installed (cycle 3, awaiting next session rotation)
- **B-009** — heartbeat noop investigation (cycle 3, finding: noop fires; cycle-0 metric was broken)
- **B-012** — ghost `access_count` SUM→MAX bug fix (cycle 16, branch `evolve/c16-access-count-max`, 31 rows clamped)
- **B-018** — agent self-reflections as primary cycle input (cycle 21, README rewrite — see below)

## 🔁 Superseded / closed

- **B-002** (per-cycle ledger.jsonl) → superseded by `cycles/*.md` + `state.json` already serving as ledger.
- **B-010** (add real noop-rate counter) → superseded by B-009 finding: empty-row query on `shell.db` is sufficient and needs no instrumentation.
- **B-011** (investigate noop gap) → superseded by cycle-4 measurement + cycle-7 finding (prompts identical, gap is reflect-rule asymmetry → addressed by B-006 + B-017).

## 🗒 Notes (not backlog items, just learned)

- **Deep-reflect IS on for umbreon already.** 13 deep heartbeats fired (vs pikamini's 12). Both agents write reflective memories — pikamini uses `kind=procedural` + topic tags; umbreon uses `tag=behavioral`. Stylistic asymmetry, not a missing feature.
