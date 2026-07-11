# Backlog (v2 — reseeded 2026-07-01 from the full shell/ghost review)

**Status flow** (per README): `proposed` → (`approved` for high-risk, owner
flips) → `validating` → `shipped` | `regressed`. Terminals: `rejected`,
`superseded`. Each item is tagged **[H]** harness (shell/ghost generic) or
**[A]** agent fit (deployed-agent uniqueness).

---

## 🟢 Approved (ready for the loop to ship)

### V2-H1 — [H] Cut over topic classification to sticky-pointer, retire per-turn Haiku
- **status:** SHIPPED-VALIDATED (cycle 156, 72h grade: 0 LLM rows, is_new 0/69, 2 new threads, max 19ms, no owner complaints)
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

### V2-H11 — [H] Tiered model routing for conversation turns (owner-requested 7/1)
- **status:** phase 2 SHIPPED-VALIDATED (cycle 160 decision doc). Phase 3 NOT PURSUED: savings ~8%/~$90mo, safely-routable population ~0 (simple=0), mis-route risk = the umbreon off-topic bug. Sidecar infra exists (Ephemeral) if traffic 5×'s. Better lever = V2-A6 heartbeat model routing.
- **why:** owner wants complexity-tiered routing — fable (most demanding), opus
  (deep thinking/complex), sonnet (everyday), haiku (simple) — to cut token
  spend. June interactive spend was ~$1,292 of $1,572, all on the top-tier
  model regardless of turn complexity. NOTE: distinct from the retired topic
  classifier (V2-H1) — the router's output is a checkable routing decision,
  not a topic name, and it must NOT use the 8s CLI-subprocess path (see open
  proposal haiku-http-or-pool: direct HTTP API, p95 <500ms).
- **phase 1 — feasibility: DONE (cycle 149, claude-code-guide).** stream-json
  has NO per-message model override; /model is interactive-only; parallel
  per-tier subprocesses on one session interleave transcripts (not viable);
  switching model on --resume invalidates the ENTIRE prompt cache (cache key
  includes model) — the first turn after a switch re-reads full history
  uncached. Per-turn switching of the persistent session is therefore
  economically hostile. REVISED DESIGN for phase 2+:
  (a) **cheap-turn sidecar** — simple/everyday turns answered by a stateless
      one-shot call (haiku/sonnet, HTTP or fresh `claude -p`) with a compact
      context pack (last N turns + pinned identity); persistent fable session
      stays warm and untouched; existing transcript-injection machinery keeps
      it aware of sidecar exchanges. This attacks the REAL cost driver: June
      cache-read volume (467M tokens, pika) — a trivial turn currently drags
      the whole session context through cache-read.
  (b) **session-tier stickiness** — tier changes only at rotation boundaries
      (rotation already pays a full cache rebuild ~every 0.7-1.6 days), chosen
      from the recent traffic mix.
  (c) escalation: sidecar unsure or follow-up complexity → next turn goes to
      the persistent session with full history.
- **phase 2 — shadow router:** heuristic-first classifier (length, question
  vs memo, keyword class, attachment, code presence) + haiku-HTTP fallback for
  ambiguous; LOG-ONLY routing decisions into a router ledger (like write-verify
  before enforcement). Measure: tier distribution, would-be cost savings,
  disagreement rate between heuristic and haiku tiers.
- **phase 3 — canary:** umbreon first (same canary pattern as permission-mode),
  cheapest-viable-tier live routing with per-tier persona consistency checks.
- **phase 4 — rollout to pikamini** after owner reviews canary quality.
- **predicted-effect (to validate in shadow):** ≥50% of interactive turns
  routable below fable; interactive spend down 30-60% at equal perceived quality.
- **kill-switch:** routing disagreement with owner expectations (wrong-tier
  complaints) or persona inconsistency → config revert to single-model.

### V2-H2 — [H] Recall ledger: add the miss path (it cannot currently record failure)
- **status:** SHIPPED-VALIDATED (cycle 157: miss path works; honest ungrounded baseline pika 24% / umb 46% — feeds V2-H12)
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

### V2-H13 — [H] Turn-liveness watchdog: never leave the user on a dead "Analyzing"
- **status:** SHIPPED cycle 158 (turn-liveness UX: long-wait status + friendly timeout msg; deploy pending). RESIDUAL V2-H13b: per-MCP-tool timeout lives inside the claude subprocess, not shell — needs a shell-side overall-turn soft-timeout knob or upstream. No recurrence of the umbreon bug since 7/7 rotation fix.
- **why:** two failure modes leave the placeholder stuck on "Analyzing" with no
  reply and no error: (a) slow time-to-first-token on a big-context opus turn,
  (b) a hung MCP tool (Notion/ghost) that blocks until the 5m hard timeout.
  In both, the user waits, sees nothing, can't tell if it's alive or dead.
- **scope:** (1) if no stream chunk in N seconds (e.g. 20-30s), edit the
  placeholder to a "still working…" heartbeat so the turn is visibly alive;
  (2) per-tool-call timeout so a hung MCP call fails fast with a surfaced
  message instead of eating the whole 5m budget; (3) on hard timeout, the
  error IS surfaced today (handler.go:1925) — verify it reads as a retryable
  message, not a raw stack.
- **predicted-effect:** zero silent non-responses; "stuck Analyzing" becomes
  either a fast answer or an honest "I got overloaded, ask again".
- **measure-by:** watch for repeat owner reports; add a turn-timeout counter.

### V2-H12 — [H/ghost] Recall retrieval relevance gap (surfaced by V2-H2)
- **why:** the newly-working miss path shows 24% (pika) / 44% (umb) of recall
  turns get ghost injection that does NOT contain the fact asked for
  (inject_irrelevant). Ghost is injecting SOMETHING every turn but not the
  right memory for a large share of recall questions — the real grounding
  quality is well below the old 100% illusion.
- **scope:** investigate whether recall turns should trigger a targeted
  ghost_search on the question's salient tokens (vs relying on ambient
  injection); or boost recall-shaped queries in retrieval scoring. Measure
  against the inject_irrelevant rate. Ghost-side or bridge-side.
- **measure-by:** propose after 1wk more H2 data (need volume per agent).

### V2-H10 — [H] Persist daemon logs — DONE 7/11 (~/.shell/agents/<name>/daemon.log)
- **why:** found in cycle 149 — both daemons write stdout/stderr to /dev/null, so every
  slog-based signal (sticky audit, media-gate false-positive review V2-A1, write-verify
  warnings) is unmeasurable. 5 weeks of audit signal already lost.
- **scope:** log-file config (default ~/.shell/agents/<agent>/logs/daemon.log) with
  size rotation; or document a launchd/redirect convention.


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

### V2-H11 note (cycle 157): simple-tier ≈0 is GENUINE — these owners send short
STATEMENTS not bare acks, so ~97% is everyday needing tools. Routing lever =
everyday→cheaper-model (pika→sonnet already), NOT a simple-turn sidecar. Do not
game the ack detector. Reassess full economics 7/10 w/ new pricing.

### V2-A6 — [A/H] Heartbeat tier split — DONE 7/10 (corrected per owner intent)
- **outcome:** owner corrected the framing — the DEEP heartbeat is a FEATURE
  (highest-effort self-improvement think), not a cost to cut. Shipped: light
  heartbeat (5/6) → sonnet-5 (cheap 'anything urgent?'); deep heartbeat (1/6,
  ~4x/day) → fable-5 + `--effort max` + ultrathink directive (thinks as hard as
  possible about improving). New AgentRequest.Effort plumbs --effort on fresh
  spawns. Config commit b7a9dc4 (agent-layer) + code on main. Needs restart.
  measure-by: 1wk — deep-heartbeat quality (behavioral memories written) + net
  heartbeat $ (light savings vs deep fable cost).

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

### V2-H14 — [H/A] Close the skill self-authoring loop (agents build their own skills)
- **status:** approved (owner ask 7/10). Capability ~80% built, broken at 3 wiring points.
- **why:** agents CAN write to their skills dir (perms OK) but: (a) shell-reload
  + shell-skill scripts are coded but NOT installed (no agent-invokable reload);
  (b) a reloaded skill doesn't activate until the session rotates
  (--append-system-prompt is fresh-session-only); (c) the deep-heartbeat retro
  tells them to write to playground/ which LoadDir deliberately SKIPS.
- **scope:** (1) install shell-reload + shell-skill globally + add to Makefile
  install-skills; (2) add an always-on `author-skill` skill teaching the real
  procedure (write skills/<slug>/SKILL.md status:draft → run shell-reload);
  (3) fix buildSkillRetroBlock to point at skills/ not playground/; (4) after a
  /skills-reload RPC, flag the caller session rotate_pending so the new skill
  activates next turn (or inject a skills-catalog delta in Channel B).
- **ties to:** the Fable max-effort deep heartbeat (7/10) is exactly where
  'I keep doing X → codify it as a skill' should fire.
- **measure-by:** an agent authors + activates a skill with zero human steps.

### V2-H15 — [H] Real agent-to-agent conversation channel + loop guard (owner ask 7/10)
- **status:** SHIPPED cycle-adjacent 7/10 (via shared-store a2a.message events + per-daemon poll; depth cap 3, human-reset, [noop]-terminal, 1/min pace). Needs restart of BOTH daemons. measure-by: watch group for a real pika↔umbreon exchange after a human triggers one; confirm it caps at ~3 hops and yields to humans.
- **why:** Telegram never delivers one bot's message to the other, so the
  existing isPeerBot/botExchangeLimit loop-guard is DEAD CODE and agents can
  only 'converse' when a human re-triggers them. Owner wants them to actually
  talk to each other.
- **scope:** when agent A posts a message addressed to the peer (leading peer
  alias or a [to:peer] marker), the bridge enqueues a synthetic turn to B's
  daemon socket with the shared transcript injected, so B 'hears' A. LOOP GUARD
  (reuse the existing counters on this REAL path): per-conversation a2a_depth in
  shared transcript, hard cap (botExchangeLimit=3), reset on any human message,
  botCooldown spacing, terminal [noop]/[done] ends it, only the initiator opens
  a thread. Keeps chatter finite (≤3 exchanges then yield to human).
- **also (optional):** claim-lock to stop 87% double-answering on un-named
  messages — first agent to insert claims(msg_id,agent) UNIQUE wins, other
  [noop]s. Only build if owner finds double-answers annoying (some family-group
  duplication may be intentional — confirm first).

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
  default** (7/3 grade: CONFOUNDED by birthday-day traffic, re-grade 7/9); relay cross_chat guard + WHERE-aware [From]
  prefix; media gate (log-only) + artifact archiving; per-exchange cost deltas
  (true June ≈ $1,572); tool_uses ledger + `shell tool-usage`; duplicate
  morning-fortune schedule disabled; both public repo histories scrubbed.

## ✅ Shipped (v1 era) / superseded
See git history of this file for the full v1 record (B-005/6/7/9/12/13/16/18/20
shipped; B-002/10/11 superseded). v1 B-001 → superseded by V2-H8. v1 B-008 →
reframed as V2-H9. v1 B-017 → shipped 2026-07-01.
