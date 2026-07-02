# Backlog (v2 — reseeded 2026-07-01 from the full shell/ghost review)

**Status flow** (per README): `proposed` → (`approved` for high-risk, owner
flips) → `validating` → `shipped` | `regressed`. Terminals: `rejected`,
`superseded`. Each item is tagged **[H]** harness (shell/ghost generic) or
**[A]** agent fit (deployed-agent uniqueness).

---

## 🟢 Approved (ready for the loop to ship)

### V2-H1 — [H] Cut over topic classification to sticky-pointer, retire per-turn Haiku
- **status:** approved (evidence overwhelming; deploy still gated on owner restart)
- **why:** cycle 148 proved per-turn Haiku classification worse on every focus
  metric; June production: is_new=1 on 1,098/1,098 Haiku calls (it NEVER
  matched an existing topic), 96-99% of ~1,000 topics single-turn, avg 8.5s
  latency ON THE USER RESPONSE PATH, ~10% error rate at 12s stalls.
- **scope:** (1) run the cycle-145 audit: sticky_matched rate over recent
  decisions; (2) if ≥80%, disable the sync Haiku call (config flag or code),
  keep cache+keyword+sticky-pointer; (3) mark deploy_pending.
- **predicted-effect:** classified-turn latency drops ~8s; topic single-turn
  ratio falls; zero fallback-haiku-error rows.
- **measure-by:** 72h after deploy via `shell-bench topic-stats`.
- **kill-switch:** stickiness or drift-catch regresses vs the cycle-148 table → revert flag.

### V2-H2 — [H] Recall ledger: add the miss path (it cannot currently record failure)
- **status:** approved
- **why:** 100% of rows since launch are grounded (no ungrounded
  classification was ever logged) and weekly volume collapsed 38→5 — the
  metric can't detect the failures it was built for.
- **scope:** verify the trigger set still fires (add same-as-yesterday
  phrasings, e.g. 跟昨天一樣); add `memory_recall`/ungrounded classification
  path; keep log-only. Enforcement is a later item.
- **predicted-effect:** ledger volume recovers toward ~30/wk; nonzero
  ungrounded rate appears (honest baseline).
- **measure-by:** 7 days after deploy.

---

## 🟡 Proposed (awaiting owner approval or more evidence)

### V2-H3 — [H] Outbound dedup ledger (scheduler-notify vs agent relay)
- **why:** duplicate-reminder class (heartbeat re-sends what a notify schedule
  sent). Deterministic guard > memorized schedule IDs.
- **scope:** record scheduler sends (chat, text-hash, ts); suppress/flag agent
  relays fuzzy-matching a send to the same chat within ±60min. Interim cheap
  version: inject "[Notify schedules firing ±1h]" into heartbeat prompt.

### V2-H4 — [H] Write-verify: check WHERE the write landed, not just THAT it landed
- **why:** meal memos verified as "written" when the Notion call patched page
  body instead of the DB property column (owner caught it 6/23).
- **scope:** for meal-context turns, classifyWrite counts only property
  patches as verified; optionally read-back verification in the correction turn.

### V2-H5 — [H/ghost] Hard-purge path for soft-deleted memories + VACUUM
- **why:** 7.6k soft-deleted rows per agent keep chunks+embeddings forever
  (~98MB orphaned in pikamini alone); no purge path exists. High-risk: data
  deletion — needs approval and backup.
- **scope:** `ghost gc --purge-deleted <age>` + optional auto; follow with VACUUM.

### V2-H6 — [H/ghost] Store embeddings as float32 BLOBs (needs owner approval: schema)
- **why:** embeddings live as JSON text (~4.5KB per 384-dim vector); BLOB is
  ~3x smaller (552MB DB → ~230MB) and faster to scan.
- **scope:** dual-read (TEXT or BLOB), write BLOB, lazy migrate on write.

### V2-H7 — [H/ghost] Adaptive compaction_suggested threshold
- **why:** fixed >500-active cutoff is permanently true at 11k memories —
  signal carries no information.
- **scope:** scale threshold to namespace size or budget-skip rate.

### V2-H8 — [H] Retire dead skill-runner telemetry path
- **why:** runner USAGE.jsonl logging never produced a single file (skills
  execute via prompt-inlined Bash, not the wrapper). The tool_uses ledger
  (shipped 7/1) now covers actual usage measurement.
- **scope:** delete or simplify the dead path in internal/skill/runner.go;
  point docs at `shell tool-usage`. Supersedes v1 B-001.

### V2-A1 — [A] Enable media_gate_enforce after false-positive review
- **why:** no-unprompted-media rule (owner order 7/1). Gate ships log-only;
  enforcement flag is per-agent config.
- **scope:** after ~1 week, count `media gate:` log lines that were actually
  legitimate sends; if false-positive rate ~0, flip `media_gate_enforce: true`
  for both agents (agent-layer repo commit). OWNER DECISION.

### V2-A2 — [A] Daily AI briefing to owner-B DM: move or retire
- **why:** $52/June into a chat with 3 replies all month — one-sided broadcast.
- **scope:** ask owner: keep / move to family group / weekly digest / kill.
  OWNER DECISION — loop only surfaces the numbers.

### V2-A3 — [A] Investigate pikamini session rotation churn
- **why:** 309 rotations in June vs umbreon 139 on similar volume (~0.7d vs
  1.6d lifespan); week-6/22 spike (121) coincided with the cost peak. Each
  rotation pays summary + cold cache.
- **scope:** find what tightened the trigger (rotate_max_tokens 60000?);
  simulate alternative thresholds from usage data; propose per-agent tuning.

### V2-A4 — [A] Reconcile skills drift: installed vs repo skills/
- **why:** repo skills/ and installed skills diverged (generate-video,
  propose-backlog, run-skill exist only installed; shell-pm/shell-tunnel/
  shell-reload only in repo). Agent-layer git (7/1) now versions installed;
  repo copies are the generic templates.
- **scope:** back-port generic skills into repo skills/ (sanitized), mark
  owner-specific ones (meal-memo, gentle-checkin) agent-layer-only, delete
  stale repo copies. Retire .evolve/skill-drafts/.

### V2-A5 — [A] Stateful health-watch memories (open/resolved)
- **why:** re-asking about resolved health issues (owner complaint). A
  health:<issue> ghost memory with status flipped on "resolved" phrasing kills
  the stale follow-up at retrieval time.
- **scope:** convention memo + reflect rule; possibly a small skill. Ships to
  agent-layer repo + ghost ns.

### V2-H9 — [H] Loud handling of legacy [relay] directives (reframes v1 B-008)
- **why:** the bridge still silently strips legacy directive text — composed
  messages vanish (last user-visible failure 4/25). Deleting the parser isn't
  enough; the failure needs to be loud.
- **scope:** on legacyDirectiveRe match: either auto-route through the relay
  or fire a bounded correction turn (reuse write_verify machinery); log count.

---

## Carried from v1 (still valid)

### B-021 — [A/H] Stop polluting agent ns with dev-session summaries *(needs approval)*
730 dev-session summaries in the agent ns via cwd-based routing. Reroute
future writes (dev:* ns or dev-session tag + suppress rule); migrate existing.
Predicted: ~30% leaner retrieval.

### B-019 — [A] Mass-tag ~900 auto-summary rows `auto-generated` *(needs approval)*
Shipped rule B-013a fires on 0 rows until this lands. Additive, reversible,
backup first.

### B-014 — [H/ghost] Prune weak edges (avg weight 0.22) *(needs approval)*
Test `GHOST_EDGE_MIN_WEIGHT=0.3` env first; mass DELETE only if env test wins.

### B-003 — [A] Per-agent transcript DB migration *(needs approval; low urgency)*

### B-015 — [A] Cross-agent fact-check convention *(observing; 1 sample)*

---

## ✅ Shipped (v2 era)

- **2026-07-01 (interactive session, pre-loop):** ghost GC FK fix; L3 topic WIP
  checkpoint committed; heartbeat task-leak filter; **B-017 heartbeat noop
  default** (v1 item, finally unblocked — measure noop rates by 7/3: baselines
  pikamini 55%/umbreon 27%); relay cross_chat guard + WHERE-aware [From]
  prefix; media gate (log-only) + artifact archiving; per-exchange cost deltas
  (true June ≈ $1,572); tool_uses ledger + `shell tool-usage`; duplicate
  morning-fortune schedule disabled; both public repo histories scrubbed.

## ✅ Shipped (v1 era) / superseded
See git history of this file for the full v1 record (B-005/6/7/9/12/13/16/18/20
shipped; B-002/10/11 superseded). v1 B-001 → superseded by V2-H8. v1 B-008 →
reframed as V2-H9. v1 B-017 → shipped 2026-07-01.
