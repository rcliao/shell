# Backlog (v2 — reseeded 2026-07-01 from the full shell/ghost review)

## 🎯 CURRENT GOAL (owner-set 2026-07-13): EVAL + QUALITY/RELIABILITY FIRST
Build toward comprehensive measurement of the harness, then use it to drive
quality/reliability. Feature work queues BEHIND measurement work.
**Priority order for the loop:**
1. **V2-H32 phase v2.0** — OwnerEval v2 deterministic core + pillar field +
   maturity ladder (L0-L4) + March-July backfill baseline (design:
   .evolve/designs/ownereval-v2.md + docs/HARNESS.md pillar map)
2. **V2-H36 phases 1-2** — testbed e2e + smoke deploy gate
3. **V2-H24** — send-gate (quality invariant)
4. **V2-H30** — self-healing reliability
5. **V2-H33 remainder** — rotation growth-based trigger code fix + TTFT split
6. Then: H20 flush, H12 recall relevance, H14 autonomous skills (H27/H29),
   H23 backups, H28 inspect, H26 remainder.
Grading rule: every ship names its eval dimension(s); validation = dimension
movement vs baseline. Pillar maturity ladder is the progress scoreboard.


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

### V2-H3 — [H] Outbound dedup ledger (scheduler-notify vs agent relay) — APPROVED 7/12
- **status:** SHIPPED cycle 162 (commit 48dc69c, DEPLOYED 7/12 11:11) — validating.
  outbound_sends ledger + SendText chokepoint suppression (60min window,
  <16-rune texts exempt); kill-switch daemon.outbound_dedup_disabled.
- **why:** duplicate-reminder class keeps recurring despite memorized-schedule-ID
  rules: 7/3 umbreon watering reminder sent twice in one message, 7/10 a 6pm reminder duplicated. Deterministic guard > memorized schedule IDs.
- **scope:** record outbound sends (chat, text-hash, ts) in a ledger; suppress or
  flag agent relays/sends fuzzy-matching a prior send to the same chat within
  ±60min. Cover BOTH observed shapes: (a) scheduler-notify + agent relay of the
  same content, (b) same content twice within one turn/message. Log-only first
  turn optional but evidence is strong — ship with suppression + loud log line.
- **predicted-effect:** zero duplicate-reminder complaints; ledger shows
  suppressed-dup count > 0 within a week.
- **measure-by:** 7 days after deploy; owner complaint recurrence.
- **kill-switch:** false-positive suppression (legit repeat message blocked) → flag off.

### V2-H12 — [H/ghost] Recall retrieval relevance gap — APPROVED 7/12 (umbreon first)
- **status:** approved (owner 7/12; volume threshold met: since 6/28 pika 11/62 =
  18% inject_irrelevant, umbreon 16/52 = 31%)
- **why:** ghost injects SOMETHING every turn but on recall-shaped turns it's
  irrelevant 18-31% of the time. Worst sources: umbreon DM shopping turns,
  hardware/troubleshooting turns, memo-edit turns.
- **scope:** start with umbreon (31%). Try targeted retrieval for recall-shaped
  turns: extract salient tokens from the question and run ghost_search on them
  (bridge-side), instead of relying on ambient injection; or boost recall-shaped
  queries in retrieval scoring (ghost-side). Measure against inject_irrelevant.
  **BOUNDARY (owner 7/12):** an independent session is improving ghost in
  parallel — this loop implements the BRIDGE-side option only and treats ghost
  as an external dependency (same for H5/H6/H7: surface data, don't edit ghost;
  hand ghost-side asks to the ghost session via owner-B).
- **predicted-effect:** umbreon inject_irrelevant 31% → <15% without hurting
  grounded rate.
- **measure-by:** 7 days of recall_verifications after deploy.

### V2-H16 — [H] A2A + scheduler "session busy" → enqueue with retry, not drop — NEW 7/12
- **status:** SHIPPED cycle 161 (commit 8a16717, DEPLOYED 7/12 09:51) — validating
- **why:** 7/12 08:28 a2a handoff FAILED with `a2a: turn failed … session for
  chat -1003731277835 thread 0 is busy` — the peer turn was silently dropped.
  Same error class hit a scheduler prompt on 7/11. Any caller that hits a
  mid-turn session currently loses its message.
- **scope:** when a synthetic turn (a2a event, scheduler prompt) hits a busy
  session, enqueue with short backoff (e.g. retry at 30s/60s/120s, then give up
  LOUDLY with a log + ledger row) instead of failing once. Bound the queue
  (1 pending per source) so retries can't pile up. Human turns unaffected.
- **predicted-effect:** a2a delivery success → 100% barring hard errors;
  scheduler "session busy" failures disappear.
- **measure-by:** next a2a collision resolves via retry (check daemon.log).

### V2-H17 — [H] Diagnose 7/12 reply truncation (post-UTF-16-fix) — NEW 7/12
- **status:** SHIPPED cycle 161 (root cause: 429 flood window drops final
  reconcile edit silently; fix commit 782380e, DEPLOYED 7/12 09:51) — validating
- **why:** 7/12 15:31 owner-A: 「pika的你回答被截掉了」. The UTF-16 CJK length fix
  shipped 6/22 (commit 1909d31), so this is a DIFFERENT truncation cause.
  Candidates: 4096-char Telegram limit on an edit path that doesn't split,
  MarkdownV2 sanitization eating a tail, streaming final-edit race, or artifact
  marker stripping.
- **scope:** pull the exact exchange from message_map/messages around 7/12 15:31
  UTC (pikamini), compare stored full response vs delivered text, identify the
  path that truncated, write a failing test, fix.
- **measure-by:** repro test passes; no truncation complaint recurrence.

### V2-H18 — [H] Latency attribution: where do >60s turns spend their time? — NEW 7/12
- **status:** step 1 SHIPPED cycle 162 (commit 06f04df, DEPLOYED 7/12 11:11):
  usage rows now carry queue_ms / ttft_ms / duration_ms. Step 2 (attribution
  analysis + rotation-churn correlation, ~7/19 after ≥1wk of rows) is the
  loop's next investigation pass.
- **why:** long-tail latency is the #1 user-visible problem: 7/1-7/12 pika p50
  18s / p95 98s (51 turns >60s = 15%), umbreon p50 24s / p95 136s (62 turns
  >60s = 21%); owner complaint 7/8 「常卡 Analyzing 沒回覆」. V2-H13 made waiting
  honest, not shorter. Unknown split between: big-context TTFT, hung/slow MCP
  tools, queue wait behind another turn, rotation summary cost.
- **scope:** (1) add per-turn phase timings to the usage/exchange record (queue
  wait, spawn/resume, time-to-first-token, tool time, stream time) — daemon.log
  exists since 7/11 so cross-check there; (2) after ~1wk of data, attribute the
  >60s population; (3) fold V2-A3: correlate with rotation events (pika 309
  rotations/June vs umb 139) and rotate_max_tokens thresholds; propose specific
  tuning as new items.
- **measure-by:** attribution table over ≥1wk of instrumented turns; each >60s
  bucket assigned a dominant phase.

### V2-H19 — [H] Vision memory: inbound photos persistent + searchable — APPROVED 7/12
- **status:** increments 1-3 SHIPPED cycle 166 (commit 9f895d3, DEPLOYED 7/12
  20:01): archive + ledger + [media-note] descriptions + Channel B injection.
  Remaining: increment 4 cross-agent dedupe (both agents archive their own
  copy today — acceptable), ghost_put of descriptions (ledger-only for now).
- **why:** owner-A is image-first (~60 image-referencing replies/month; multi-day
  photo troubleshooting threads) but inbound photos land in os.CreateTemp and
  expire — user-visible failure 7/11: photo temp file expired mid-thread (pika
  answered from umbreon's description). Outbound artifacts are archived;
  owner photos are the only media shell discards. Nothing visual reaches
  ghost or the transcript beyond "(photo)"/caption.
- **design (owner-agreed 7/12): SHELL-side; ghost consumes via existing text
  API — no ghost core changes (parallel ghost session stays unblocked).**
  1. Archive + ledger: media/YYYY-MM/<file_unique_id> under the agent dir; DM
     photos per-agent, GROUP photos in the shared store dir (no-peeking rule
     preserved). New media table: chat, msg_id, file_id, path, caption,
     description, ts. Retention ~90d; folds into H23 backups.
  2. Describe-on-capture: v1 = in-turn `[media-note]` marker the agent emits
     (zero cost, prompt-contract); bridge parses it into the ledger +
     ghost_put (semantic, tags media + chat:<id>). Upgrade to async haiku
     vision one-shot ONLY if marker compliance is poor (off response path —
     NOT a V2-H1 latency repeat).
  3. Re-readable: Channel B injects the chat's last N media rows (path +
     description + age) → 「剛剛那張照片」 = agent Reads the archive.
  4. Cross-agent: group photos answered from the shared archive, not the
     peer's secondhand description.
- **measure-by:** zero temp-expired apologies; photo threads survive rotation;
  recall grounding on photo-referencing turns.

### V2-H20 — [H] Pre-rotation memory flush — APPROVED 7/12
- **status:** approved (owner 7/12). Ship after validation window (~7/19).
- **why:** sessions rotate every ~0.7-1.6 days and whatever wasn't explicitly
  saved is reduced to a summary; honest recall baseline is 24-46% ungrounded
  (V2-H2). OpenClaw's pre-compaction flush is the right pattern: one bounded
  "save anything worth remembering to ghost now" turn BEFORE rotation.
  Write-side complement to V2-H12 (read-side) and B1 (same-day distillation).
- **scope:** hook the rotation decision point: before killing the session, run
  a bounded flush turn (cheap model OK; ephemeral profile) with a strict
  contract (ghost_put what matters: open threads, commitments, new facts;
  [noop] if nothing). Cap runtime; rotation proceeds regardless of flush
  outcome. Log flush results (count of memories written) for measurement.
- **measure-by:** ungrounded recall rate trend post-deploy (target: pika 18%→
  <10%, umb 31%→<20% combined with H12); flush writes >0 on most rotations.

### V2-H24 — [H] Bridge send-gate: never deliver internal text — APPROVED 7/12
- **status:** approved (owner 7/12). Queue after current validation window.
- **why:** 40 instances (6/2-7/8) of internal monologue ("this message is for
  Umbreon, I'll stay quiet" — SENT to the group), stale background-task
  musings replacing answers, and 5 raw API errors delivered as replies (6/13:
  owner-A reports a health symptom, gets a model-error string).
- **scope:** final-reply gate in the bridge before send: (a) error-shaped text
  → friendlyTurnError path, never raw; (b) peer-deference/meta monologue
  (English-when-user-Chinese + meta-phrase patterns) → suppress as [noop];
  (c) background-task musing on a user-question turn → retry once with a
  "answer the user's question" correction turn (reuse write-verify machinery),
  else friendly fallback. Log every gate hit (ledger row) — kill-switch flag.
- **measure-by:** gate-hit count > 0 with zero false suppressions of real
  answers (spot-check ledger weekly); zero new leaked-monologue incidents.

### V2-A7 — [A] Port pika's brevity profile to umbreon — APPROVED 7/12
- **status:** approved (owner 7/12) — EXECUTED same day in agent-layer repo
  (see commit there); umbreon daemon restarted.
- **why:** pika verbose-to-casual 3.5% (July DM) vs umbreon 15-19%; umbreon
  apologizes 2.4× more. The 6/18 brevity contract landed in pika's prompt
  only (or stronger there).
- **measure-by:** umbreon verbose-to-casual rate → <8% by 7/26; apology rate
  trend.

### V2-A8 — [A] Verify-before-assert contract for spec/entity claims — APPROVED 7/12
- **status:** approved (owner 7/12) — EXECUTED same day (prompt-level, both
  agents, agent-layer repo).
- **why:** the #1 trust-erosion pattern: ~10 confident-wrong-fact corrections
  per agent per week (Zyren AUTO mode asserted wrong for a WEEK — ; nonexistent restaurant; wrong store location; guessed
  purchase identity).
- **scope:** prompt contract: product specs / store facts / "which one did
  you buy" claims require a fresh lookup (manual, listing, web search, or an
  actual memory/Notion read) OR an explicit unverified-marker (「我沒查證，
  印象中是…」). Never present inference as fact for checkable claims.
- **measure-by:** corrections-per-week trend (baseline ~10/agent); target <4
  by 8/1. If prompt-level insufficient, escalate to a mechanical check.

### V2-H28 — [H] `shell inspect`: agents can introspect their own harness — APPROVED 7/12
- **status:** approved (owner 7/12). Queue after validation window.
- **why:** 66 failed Bash calls in 10 days hunting for skill-script paths and
  querying schedules with wrong column names; umbreon ghost_get 73% fail;
  pika told owner-A it 「沒辦法直接查排程紀錄」 when she reported the dup-reminder
  bug. The agents' own runtime is a black box to them.
- **scope:** read-only `shell inspect` subcommands exposed to agents:
  schedules (with next-run), recent outbound sends (dedup ledger), session
  state, tool-failure tallies, media ledger (post-H19). Plus: ensure skill
  scripts are reliably on PATH (ties into V2-H14 wiring fix). Document in the
  system prompt so discovery is instant, not a find-expedition.
- **measure-by:** failed path-hunting Bash calls → ~0; agents answer
  harness questions (schedule contents, "did that send twice?") directly.

### V2-H25 — [H] Transcript retention: archive, don't prune — APPROVED 7/12
- **status:** BLEED-STOP SHIPPED + DEPLOYED same evening (commit cde25b3,
  18:33 PT): the hardcoded 7-day prune (daemon 6h tick) is now config-driven
  `store.message_retention_days`, default 365, negative = never. Remaining
  scope for the loop: monthly archive DBs if a year of rows ever hurts perf.
- **why:** `messages` retains only ~1 week (June is GONE); the shared
  transcript store has no human-side rows since March. Every analysis, B1
  distillation, and "what did we say last month" is impossible. The family
  transcripts are a primary asset being silently deleted weekly.
- **scope:** find the pruning path (rotation cleanup?); replace delete with
  archive (monthly attach-DB `messages_archive-YYYY-MM.db` per agent, or
  retention window ≥12mo in place). Backfill nothing (data's gone) but stop
  the bleeding IMMEDIATELY — this is the most time-sensitive approved item:
  every week of delay loses a week of history.
- **measure-by:** rows older than 30d still queryable after multiple rotations.

### V2-H23 — [H] Nightly SQLite backups — APPROVED 7/12
- **status:** approved (owner 7/12 evening).
- **why:** no automated backup of shell.db / memory.db / transcript DBs; the
  memory IS the product. Ad-hoc .bak files exist only around risky changes.
- **scope:** nightly launchd or scheduler job: `sqlite3 <db> ".backup"` for
  every agent DB + shared store → ~/.shell/backups/YYYY-MM-DD/ with 14-day
  retention (+ weekly kept 8 weeks). Verify backup readability (integrity
  check). Coordinate with H25 archives and H19 media dir.
- **measure-by:** restorable backup exists for every DB, verified weekly.

### V2-H26 — [H+ghost-boundary] Memory provenance + audience scoping — APPROVED 7/12
- **status:** bridge-side SHIPPED cycle 166 (commit 9f895d3, DEPLOYED 7/12
  20:01): chat:<id> provenance on StoreDirective/inline/behavioral writes +
  PUBLIC-POST CONTRACT injected on system turns to groups. Remaining:
  retrieval-side scope filtering (ghost session's domain) + retro-tagging
  existing DM memories (best-effort, propose separately).
- **why:** 7/5 privacy leak — a fact owner-A told ONLY pika (DM) surfaced in
  umbreon's public 月光下的小思考; zodiac wrong in 占卜 (7/10); dove-nest
  state written from stale data (7/6). Memories carry no "who said it, in
  which chat" and scheduled composers don't check scope or freshness.
- **scope (bridge-side; ghost scoring = ghost session's domain):** (1) bridge
  memory writes always tag source chat + speaker (chat:<id>, from:<name>);
  (2) scheduled composer prompts (diary, 占卜, 小思考, briefing) get a hard
  contract: public posts may use ONLY group-scoped or explicitly-shareable
  memories; person-facts (birthdays, signs) must be verified against pinned
  before publishing; date-sensitive states need a freshness check;
  (3) retro-tag existing DM-sourced memories where derivable (best-effort).
- **measure-by:** zero cross-scope leaks; zero wrong person-facts in
  scheduled posts.

### V2-H30 — [H] Self-healing session recovery (reliability) — APPROVED 7/12
- **status:** approved (owner 7/12 evening — owner framing: "improve the
  reliability of the shell system").
- **why:** the 401 stale-OAuth class requires owner-B to manually `shell session
  rotate/kill` while agents serve errors; restart leaves broken-pipe
  persistent-proc corpses (2 on 7/12); SQLITE_BUSY hit the scheduler (7/11);
  rate-limit windows surface as raw "session limit" text in heartbeat output
  (7/12 23:04).
- **scope:** (1) detect auth-failure (401/OAuth) turn results → auto-kill +
  respawn the persistent proc and retry the turn once before surfacing an
  error; (2) detect rate-limit responses → defer heartbeats/scheduled prompts
  past the stated reset time (don't burn the window), interactive turns get a
  short honest reply; (3) busy_timeout pragma on every sqlite open; (4) on
  startup/restart, reap orphaned persistent procs cleanly (no broken-pipe
  noise); (5) ledger row per self-heal event so reliability is measurable.
- **measure-by:** zero manual session-rotate interventions; zero raw
  limit/auth error text reaching any chat; self-heal ledger shows
  detect→recover pairs.

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

### V2-H10 — [H] Persist daemon logs — DONE 7/11 (~/.shell/agents/<name>/daemon.log)
- **why:** found in cycle 149 — both daemons write stdout/stderr to /dev/null, so every
  slog-based signal (sticky audit, media-gate false-positive review V2-A1, write-verify
  warnings) is unmeasurable. 5 weeks of audit signal already lost.
- **scope:** log-file config (default ~/.shell/agents/<agent>/logs/daemon.log) with
  size rotation; or document a launchd/redirect convention.


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

### V2-H8 — [H] Skill-usage telemetry — REFRAMED 7/12, absorbed into V2-H14
- **was:** "retire dead runner USAGE.jsonl path". **Now:** the dead stats are
  actively harmful — the deep-heartbeat retro shows "0 runs" for skills used
  daily, which taught umbreon to distrust the retro entirely (7/11 deep
  reflection). Fix = feed skill.Rollup from the tool_uses ledger (matches
  bash invocations by skill path). Shipped as part of V2-H14 scope (3).

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

### V2-A3 — [A] Pikamini session rotation churn — FOLDED INTO V2-H18 (7/12)
- superseded: rotation-churn analysis is step 3 of the V2-H18 latency
  attribution investigation (approved 7/12).

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
- **status:** approved (owner ask 7/10; workshop-gating amendment 7/12; FULL
  DIAGNOSIS 7/12 evening — see below). Priority ELEVATED: owner wants H27
  (meal-log v2) and H29 (shopping research) authored BY THE AGENTS as this
  item's first two test cases, not built as shell skills.
- **why (original 3 wiring points):** (a) shell-reload + shell-skill scripts
  coded but NOT installed; (b) reloaded skill inert until session rotation;
  (c) deep-heartbeat retro points at playground/ which LoadDir SKIPS.
- **why (7/12 trace — why the reflect cycle never authors):**
  (d) **INPUT IS DEAD DATA**: the retro shows per-skill usage stats from
      runner USAGE.jsonl which never wrote a file — umbreon's 7/11 deep
      reflection literally reasoned: "Skills are all lazy/0-hot-budget; the
      '0 runs' counter doesn't capture bash-script invocations… so no
      retiring warranted." The agent engaged the retro, found the evidence
      broken, and rationally did nothing. Fix = V2-H8 REFRAMED: feed
      skill.Rollup from the tool_uses ledger (which DOES see bash
      invocations) instead of deleting the path.
  (e) **BUDGET CROWDING**: the deep prompt stacks consolidation ("highest
      priority") + tasks + learnings + retro + a multi-part behavioral
      self-audit into one turn; authoring is the last menu item and loses
      every time. Fix: rotate the deep-heartbeat focus (1-in-N deep beats =
      WORKSHOP beat where skill authoring IS the primary task).
  (f) **RATE-LIMIT STARVATION**: pika's 7/12 23:04 deep beat output was
      "You've hit your session limit · resets 4:30pm" — fable+max deep beats
      compete with interactive traffic for the subscription window. Needs
      limit-aware scheduling (defer deep beat when limited, don't burn it).
  (g) **NO SUCCESS SIGNAL**: nothing ever told an agent "your draft is now
      live" — no reinforcement that authoring works. Workshop approval +
      activation notice closes the feedback loop.
- **scope (REVISED 7/12: AUTONOMOUS activation — owner explicitly waived the
  human-approval gate; guardrails are mechanical, veto is after-the-fact):**
  (1) install shell-reload + shell-skill + add to install-skills;
  (2) always-on `author-skill` skill teaching the procedure: write
      skills/<slug>/SKILL.md → run shell-reload → skill is LIVE next turn;
  (3) fix retro: point at skills/ (not playground/), feed stats from
      tool_uses (absorbs V2-H8);
  (4) **activation lint (mechanical gate, replaces human approval)**: valid
      frontmatter; token size cap per skill + total self-authored hot budget
      cap; allowed-tools must be a SUBSET of the agent's existing permissions
      (a skill can never escalate perms); reserved names blocked; lint fail →
      draft stays inert with a reason the agent can read and fix;
  (5) skills-catalog delta in Channel B / rotate_pending → next-turn
      activation, plus a confirmation the agent SEES (closes feedback loop);
  (6) **notify-and-revoke, not approve**: activation notice to owner-B
      (debugging topic 1650) with `shell skill retire <slug>` as one-command
      veto; every change auto-committed to the agent-layer git repo so any
      revert is trivial;
  (7) **auto-retire**: self-authored skill unused (tool_uses) for 14d or
      correlated with a correction spike → auto-demote to .archive with a
      note in the next deep reflection;
  (8) workshop beat: every Nth deep heartbeat makes authoring the PRIMARY
      task, seeded with recurring-pattern candidates (H27 meal-log v2, H29
      shopping-research); must be limit-aware (defer, don't burn, when the
      subscription window is exhausted).
  Residual risk accepted by owner: self-modifying prompt via self-authored
  skills. Mitigated by no-perm-escalation lint + size caps + git history +
  one-command revoke; prompt-injection-laundering (web content → skill text)
  is the one to watch in review — flag skills whose text quotes external
  URLs/content in the activation notice.
- **first test cases (owner 7/12):** H27 — extend pika's EXISTING meal-memo
  skill (already loaded, tier:hot, food-log script in daily use) with
  day-scoped correction ops (「加到晚餐不是早餐」 as one op) + hard write
  receipt; H29 — umbreon authors shopping-research. Agents draft, owner-B
  approves.
- **measure-by:** an agent-authored draft reaches live activation through the
  workshop gate; H27/H29 exist as agent-authored skills by ~8/1.

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
- **also (optional):** claim-lock to stop double-answering — **REJECTED by
  owner 7/12**: both agents responding in the group is intentional/fine.
  Do not build; drop the claim-lock idea from future proposals.

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

### V2-H31 — [H] Owner-eval: owner-fitness scorecard — SHIPPED cycle 167 (7/12)
- **status:** SHIPPED (commit 4fcf015). `shell eval [--since 168h] [--json]`
  per agent config. 12 dimensions split harness/agent; detectors grounded in
  the 7/12 mining phrases with negative-control tests; numbers-only output
  (redaction-safe by construction — snapshots live in ~/.shell/eval/, only
  aggregate numbers may enter this repo).
- **baselines (7/05-7/12):** pika — recall 80%, confab 11.6%, verbose 13%,
  corrections 1.5%, leaks 0.9%, p95 81.5s, >60s 14.3%. umbreon — recall 63%,
  confab 22.2%, verbose 14.8%, corrections 1.4%, nudges 1.1%.
- **protocol:** the loop runs `shell eval` every cycle; every ship names the
  dimension(s) it should move; validation = dimension movement vs these
  baselines, NOT vibes. Bad numbers are reported as-is — the metric's job is
  honesty, not reassurance.
- **future:** unverified-claim rate needs a checkable-claim detector (hard
  LLM-free; A8's effect shows up in factual_corrections for now); photo
  dimensions activate as H19 data accumulates.

### V2-H32 — [H] OwnerEval v2: trend-first comprehensive harness eval — APPROVED 7/12 (design complete)
- **status:** design SHIPPED (.evolve/designs/ownereval-v2.md, ultracode
  workflow 7/12: 5 transcript-mining lenses + 4 peer-research agents +
  synthesis + 2 adversarial critics). Implementation phased: v2.0
  deterministic core → v2.1 trend engine + cross-DB → v2.2 judge tier.
- **headline mining findings (numbers only; details in private
  ~/.shell/evolve-reviews/):** complaint rate +53% into July while gratitude
  fell ~64%; question re-sends grew ~49x March→June; resignation-shaped
  markers tripled; thread abandonment after dissatisfied endings doubled in
  July; engagement depth −40-55% off mid-June peak (retention intact —
  shallower, not churned); owner-B DM flatlined; agent-switching after
  failures grew 5x. v1's flat correction rate masked ALL of this.
- **peer finding:** NO surveyed harness (OpenClaw/Hermes/OpenHuman/field)
  measures owner satisfaction longitudinally — behavioral-CSAT from a private
  deployment's own transcripts is novel territory.
- **also files 7 harness prerequisites** (design §6): usage.model attribution
  on long-lived sessions, sender attribution on group message_map rows,
  shared exchange id usage↔message_map, per-message timestamps, outbound
  message-id retention + schedule-id stamping, duration_ms population check,
  degenerate tier-router investigation.
- **measure-by:** v2.0 definition-of-done in design; March-July backfill
  baseline pass before any goal enforcement.

### V2-H33 — [H] Rotation-thrash fix + speed program — APPROVED 7/13 (incident)
- **status:** approved (owner 7/13, live incident: 2-minute single-question
  turns). CONFIG MITIGATION DEPLOYED 7/13 morning (agent-layer commit):
  rotate_max_tokens 60k/90k → 300k, rotate_max_context_tokens 500k/350k →
  250k. Code fix + program below for the loop.
- **root cause:** the token-based rotation trigger compares CUMULATIVE
  input+cache_creation against rotate_max_tokens, but a fresh generation's
  FIRST turn creates the entire base context (~90-170k, grown past the 60k
  threshold as prompt/pinned/skills grew) — so every generation was flagged
  for rotation on turn one. Generations lived MINUTES (observed age 3m15s);
  every turn paid rotation summary (~48s) + full cache re-creation (40-90s).
  V2-A3's June churn (309 rotations/mo) was the early symptom.
- **scope (code fix):** rotation trigger must measure GROWTH since generation
  start (cumulative minus first-turn creation), not absolute totals; startup
  validation must error (not warn) when rotate_max_tokens < observed base
  creation; alert on low generation age.
- **eval tie-in:** add `generation_age_p50` (harness, SQL) to OwnerEval v2 —
  rotation thrash becomes a graded regression, never a mystery again. Grade
  this fix by: cold-turn share falling, latency_p95 < 60s, generation age
  p50 > 12h.
- **speed program status (7/13 afternoon):** SHIPPED same day — (1) eager
  rotation + cache pre-warm (bridge PrewarmDueSessions, 10-min daemon tick,
  idle-guarded; warm-up turn source=prewarm, excluded from memory exchange
  log); (3) answer-first-persist-after + casual-turn no-deliberation pinned
  as behavioral-speed-contract in both agents; (4) instant 👀 receipt
  reaction pre-lock. REMAINING for loop: (2) first-event vs first-text TTFT
  split (diagnostic); NOTE: handler per-chat lock wait happens BEFORE
  manager.Send so it is INVISIBLE to H18 queue_ms — 🕐 reaction marks it;
  fold a handler-level timestamp into the TTFT-split item.
- **combined 7/13 speed fixes:** rotation thresholds (morning) + Notion MCP
  removal (~30-40k base, V2-H34) + eager prewarm + speed contract. measure:
  normal-message time-to-first-content <15s; cold-turn share ~0 during
  waking hours; latency_p95 <60s.

### V2-H34 — [H/A] Notion skill replaces Notion MCP server — SHIPPED 7/13
- **status:** SHIPPED + DEPLOYED 7/13 (repo: skills/notion + write-verify
  precision; agent-layer: skill installed, meal-memo updated, notion MCP
  disabled; daemons restarted — mcp.json now ghost+shell-bridge only).
- **why:** the official Notion MCP server's tool schemas cost ~30-40k tokens
  of base context per session — the single largest lump in the 48-77k base
  that caused the V2-H33 rotation thrash. The deployment uses ~4 operations.
- **what:** `skills/notion` REST script (get-page, query-db with
  type-adaptive filter, patch-prop with READ-BACK receipt, append). Validated
  live against the real food-log DB (read path). patch-prop's read-back line
  doubles as the write-verification receipt (H27 synergy). write_verify Bash
  matcher tightened: notion reads no longer count as persistence.
- **measure-by:** cache_create on fresh spawns drops ~30-40k (watch next
  rotations); base_context_tokens dimension (V2-H33) trends down; meal-memo
  writes keep verifying (write_confabulation must not rise); zero notion
  tool failures in tool_uses.

### V2-H35 — [H] Context manifest instrument + pinned audit — SHIPPED 7/13
- **status:** SHIPPED + DEPLOYED. `shell context [--chat N] [--full]` reads the
  LIVE composed system prompt from the running daemon (new GET /context RPC →
  Bridge.ContextManifest) with per-component char/token sizes.
- **first-run findings (both agents):**
  1. Shell-authored Channel A ≈ only 8k tokens (pinned ~3.7k, skills ~2.9k,
     group ~1k, lifecycle/timestamp small) — the rest of cache creation is
     CLI baseline + MCP schemas. Bounds what prompt-trimming can achieve.
  2. `identity` component = 0 by design (persona lives in pinned identity
     memories; agent.system_prompt intentionally empty). Documented.
  3. agent-B pinned audit: one lesson stored as FIVE pinned post-mortems
     (agent-A: diary date grounding) → consolidated to ONE rule, originals
     unpinned (~800 tokens reclaimed).
  4. **NAMESPACE DRIFT (serious):** 10 memories incl 4 pins lived in a dead
     ns variant the profile never loads — among them the 7/5 privacy-scope
     lesson (never injected!) and trip/allergy details. Migrated to the live
     ns; completed-op residue unpinned. ROOT CAUSE: direct ghost CLI writes
     with a hand-typed wrong -n; a 309-token pinned "remember your ns" memory
     demonstrably did NOT prevent it → mechanical fix belongs ghost-side
     (reject/warn unknown ns on put) — HANDED TO GHOST SESSION via owner.
- **eval tie-in:** base_context_tokens (V2-H33) reads the manifest TOTAL;
  add pinned_dead_ns_count as a ledger-liveness-style check.
- **measure-by:** manifest TOTAL trend weekly; zero dead-ns pins.

### V2-H36 — [H] Testbed: agent-driven e2e harness eval — APPROVED 7/13
- **status:** approved (owner 7/13). Priority slot 2 under the eval-first goal.
- **why:** production eval detects failures AFTER the family experiences
  them — the owners are currently the QA department. The harness's promises
  are mechanically verifiable, so they can be proven pre-deploy.
- **design:** `shell testbed` builds the FULL real stack (store, ghost,
  claude subprocess, skills, rotation, ledgers) for a disposable test agent
  (~/.shell/testbed/, cheap model, fresh DBs per run) with a CAPTURE
  transport replacing Telegram — delivery reconciliation = generated vs
  captured, exact. Bridge already has the Transport interface; routing e2e
  tests already construct the stack directly.
- **phases:** (1) testbed runtime + capture transport + memory-round-trip
  scenario (send fact → assert write ledger → force rotation → recall →
  assert grounded); (2) smoke suite (delivery/truncation/dedup/error-gate)
  + `make testbed-smoke` as the pre-SIGHUP deploy gate; (3) dual-agent a2a +
  media round-trip scenarios; (4) `claude -p` adversarial examiner (mission:
  make it drop/contradict/confabulate/leak; confirmed finds frozen into the
  scripted suite) + realistic-user driver (bilingual, rapid-fire, photos,
  corrections). Examiner budget-capped like the judge tier.
- **rails:** haiku-class test agent, hard turn caps, fully separate DB tree
  (assert it never touches family data), scenarios model-independent by
  design (they test the seam, not the soul).
- **eval tie-in:** maturity L4 requires production goals held AND e2e
  scenarios green. Testbed proves promises; production eval measures
  experience.
- **measure-by:** memory-round-trip scenario runs green end-to-end; smoke
  gate blocks a deliberately-broken build.

### V2-H33 addendum — 7/13 midday: the 82-second arithmetic turn
- **finding:** a 4-word arithmetic answer took 82s wall: ~40s BRIDGE PREWORK
  (invisible to H18 — timer starts at manager.Send) + ~40s silent model
  thinking (no tools) + ~1s generation. Prewarm itself is working (background
  rotations 52-67s, invisible to owners).
- **shipped:** (a) prework instrumentation — bridge logs turn prework_ms >2s;
  handler logs Telegram receipt lag >3s. The full pipeline is now visible:
  receipt lag | prework | ttft | stream. (b) `model_routing.
  conversation_effort` — spawn-bound --effort for conversation turns;
  CANARY: agent-A at "low" (cut silent deliberation), agent-B unchanged.
  Grade by TTFT distribution + factual_corrections (quality must hold).
- **next if insufficient:** identify the 40s prework consumer from the new
  logs (suspects: ghost context assembly, transcript injection); and REVISIT
  the ephemeral fast-path sidecar with the LATENCY lens — cycle 160 rejected
  it on cost grounds ($90/mo), but it remains the only path to ~2-4s total
  for simple turns. Owner expectation is anchored at "2 seconds".
