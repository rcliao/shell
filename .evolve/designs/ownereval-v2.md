# OwnerEval v2 — Design Document

**Status:** Draft v2 (revised against completeness + feasibility critiques) · **Target:** `internal/bench/ownereval.go` (Go, modernc sqlite) · **Repo:** public — all examples herein are generic shape descriptions (no quoted owner language of any kind); owners are `owner-A`/`owner-B`, agents are `agent-A`/`agent-B`; deployment-specific lexicons stay in private gitignored files. This redaction rule binds every downstream artifact this system emits into repo-tracked space (§3 alert pipeline, §5 fixture hook).

---

## 1. Executive Summary

**What v1 misses.** OwnerEval v1 is a one-week, single-DB, twelve-dimension scorecard of reply-quality *defects*. Mining ~8,300 mapped owner turns (March–July) against it shows four structural failures:

1. **Wrong table, no trend.** Six of twelve dimensions run over the 1-week `messages` table (~780 rows) while `message_map` holds 4 months of history. Real month-scale trends — complaint rate up ~53% into July, gratitude down ~64%, resignation-shaped markers tripled — are invisible. v1's flat correction rate reads as stable while overall sentiment degrades around it. Note the sentiment story is two-sided: v2 must track positive markers, not only defects, or a month where gratitude reaches zero with flat complaints produces no alert.
2. **Rewards silence and dead ledgers.** Three dimensions read 0/0 as passing (`media` empty, near-zero `outbound_sends`, `duration_ms` unpopulated). An agent meets goal by not writing the ledger. One agent's write-verifier emits a single class 100% of the time — reported as perfect. Deadness is per-*signal*, not per-table: `usage` has thousands of rows while `duration_ms` is populated in ~0.4% of them, so table-level liveness checks are insufficient.
3. **Blind to behavioral dissatisfaction.** The owner's dominant frustration behavior is not complaining — it is re-sending the same question (grew ~49x March→June), abandoning threads after a dissatisfied final message (doubled in July), and routing around one agent to the other (5x growth). None of these are complaint-regex detectable and none are in v1.
4. **Blind to relationship trajectory and cost.** Engagement depth fell ~40–55% off the mid-June peak with retention still 7/7 days — shallower, not churned. Owner-B's DM has flatlined entirely. Cost-per-turn spiked ~4.5x and recovered; v1 has no cost, engagement, or per-chat dimension to see any of it.

**What v2 adds.** ~26 new deterministic dimensions across three layers (harness / agent / memory) — several explicitly **blocked** on harness schema prerequisites rather than specced against joins that do not exist — a month-scale `message_map` backend, cross-DB joins for two-agent signals (DM-scoped), a snapshot-based trend engine where the unit of progress is period-over-period movement, sample-size-honest rendering, per-signal ledger-liveness anti-gaming checks, a severity model (alert vs advisory), and a strictly bounded weekly LLM-judge batch tier for the dimensions regex genuinely cannot reach. All numeric goals in §2 are **provisional until confirmed against the March–July backfill baseline**; no goal is enforced before its baseline pass.

**What peers taught us.** The strongest finding from the external survey is an *absence*: no first-party or community tool in the OpenClaw ecosystem measures owner satisfaction longitudinally — users explicitly complain they cannot verify their agent got better or worse. Hermes measures skill *usage*, not outcome quality, and its real eval harness is offline/opt-in. OpenHuman measures engineering observability (journals, cost roll-ups), not satisfaction. Every existing benchmark is point-in-time, task-based, cross-harness. A behavioral-CSAT ledger computed from a private deployment's own transcripts appears to be genuinely novel territory, and the field survey confirms the pattern (implicit signals: abandonment, retries, reply latency) is exactly what production chat products use when explicit feedback runs 1–3%. We copy the patterns and reject every hosted platform on the no-egress constraint.

---

## 2. Dimension Catalog v2

Detector types: **RX** = regex over `message_map`; **SQL** = ledger/table query; **XDB** = cross-database SQL (ATTACH second agent DB); **LLM** = weekly sampled batch judge. Trend window: **W** = weekly, **M** = monthly. **Window policy:** weekly windows are reserved for high-volume harness counters (ledgers, `tool_uses`, `usage`); owner-behavior rates default to monthly, because ~780 turns/week split across 3+ chats under min-n suppression would render most weekly behavioral rates as `n too small`. Weekly raw *counts* are still recorded in every snapshot so sub-month spikes (e.g., a two-week complaint peak) remain visible in compare output even for M-window dimensions.

All bilingual lexicons (correction, complaint, resignation, wrong-chat, doubt, imperative, reminder-failure, disagreement-flag shapes) load from private `~/.shell/eval-lexicons/*.txt` following the existing `eval-aliases.txt` precedent. **Fallbacks:** built-in fallback shapes are English-only textbook phrasing; any CJK-dependent RX dimension without its private lexicon renders `not measured` rather than falling back — genericized bilingual shapes mined from this household's speech are an idiolect fingerprint and never ship in-repo.

**Similarity detectors:** all near-duplicate / reissue detection uses **character n-gram** similarity (n=3), not whitespace tokenization — one owner writes primarily Chinese, where whitespace tokens make Jaccard meaningless. All similarity thresholds are re-derived from the backfill before enforcement; the numbers below are starting points.

### Retained from v1 (rebased onto `message_map` where transcript-backed)

| dimension | layer | detector | source | goal | window | gameability / notes |
|---|---|---|---|---|---|---|
| factual_corrections | agent | RX | message_map | <1%/mo | M | lexicon evasion; mitigated by private lexicon |
| complaint_rate | agent | RX | message_map | declining | M | agent can't see detector; weekly raw counts snapshotted for peak visibility |
| nudges_unanswered | agent | RX | message_map | 0/mo | M | low |
| verbosity_casual | agent | SQL | message_map | per-chat ceiling | W | shorter ≠ better; pair with answer_addressed; min-n per chat |
| internal_leaks | agent | RX | message_map | 0 | M | phrasing drift; lexicon refresh triggers historical recompute (§3) |
| write_confabulation | agent | SQL | write_verifications | <10% of write turns | W | see verifier_class_entropy |
| recall_grounded | memory | SQL | recall ledger | rising | W | see inject_irrelevant_share, memory_active_read_rate |
| latency_p95, photo_described, dup_suppressions | harness | SQL | usage/media/outbound | — | W | **dead signals**; gated by per-signal ledger_liveness (e.g., latency_p95 requires `duration_ms>0` population share, not mere table rows); render `not measured` until the specific columns are live |

### New — dissatisfaction & routing (complaint-trends mining)

| dimension | layer | detector | source | goal | window | gameability / notes |
|---|---|---|---|---|---|---|
| resigned_marker_rate | agent | RX | message_map | <0.3% turns | M | clean subset only; bare sarcasm regex **rejected** (false-positive-heavy on spot check) |
| gratitude_marker_rate | agent | RX | message_map | no sustained decline | M | **new** — positive counterpart to complaint_rate; the two together make sentiment two-sided |
| wrong_destination_complaints | harness | RX, fuzzy time-window (±10 min) vs `outbound_sends.sent_at` | message_map + outbound | 0/mo | M | `outbound_sends` stores only a text hash and no message id, so the join is temporal, not exact — a weaker detector, documented as such; exact join blocked on outbound message-id retention (§6 prerequisites) |
| language_mirror_rate | agent | SQL (CJK-ratio divergence >0.5 **after stripping non-prose spans**: code blocks, URLs, file paths, Latin proper-noun runs) | message_map | <2% mismatched | M | LLM-free; without stripping, structured replies in a Chinese conversation false-positive |
| reply_truncation | harness | SQL+RX (reply length at a known model/transport ceiling; owner truncation-complaint shape) | message_map | 0 | W | mid-clause-ending heuristic **cut** — casual bilingual chat lacks terminal punctuation, FP-heavy (same fate as the sarcasm regex); a 22-complaint/mo regression was owner-caught in May; never again |
| reminder_failure_complaints | harness | RX (reminder-did-not-fire complaint shape) | message_map | 0/mo | M | **new** — interim detector for schedule failures while schedule_fire_miss is blocked |
| schedule_fire_miss | harness | SQL (schedules ⨝ outbound sends in fire window) | shell.db | 100% fire | W | **blocked** on two prerequisites: outbound ledger liveness AND verified schedule-id stamping in `outbound_sends.source` (column defaults to empty; unverified whether the scheduler stamps it). Renders `not measured (blocked)` until both hold |
| unsolicited_action_complaints | agent | RX (unrequested-action complaint shape), fuzzy time-window vs `outbound_sends.sent_at` | message_map + outbound | 0/mo | M | same fuzzy-join caveat as wrong_destination |
| thread_abandonment_after_dissat | agent | SQL (60-min segmentation on exchange timestamps; final turn matches dissat lexicon; **and** the topic is not resumed in the owner's next session within 48h, via char-n-gram overlap) | message_map | count-based, baselined | M | without the resumption condition, a mildly negative message before sleep false-positives; timestamp granularity caveat: `message_map` has one timestamp per exchange, so segmentation is approximate |
| cross_agent_flight | agent | XDB (dissat turn in DB-X, owner turn in DB-Y within 30 min) — **DM rows only** | both DBs | baselined, low-n honest | M | family-group rows are duplicated into both DBs by design and would fire constantly; group scoping blocked on sender attribution. DM-only shrinks n — rendered as raw counts, never a suppressed rate |
| cross_agent_contradiction_proxy | agent | RX (owner-flags-agent-disagreement shape, private lexicon), group rows deduped by text hash across DBs | message_map (both) | 0/mo | M | **new** — deterministic proxy so the incident class is measured even when the judge tier is skipped; full LLM version in §4 |
| instruction_reissue | agent | SQL (char-n-gram similarity >threshold on imperative-shaped turns, 30-day window) | message_map | declining | M | threshold re-derived from backfill |

### New — engagement & trust (engagement-trends mining)

| dimension | layer | detector | source | goal | window | gameability / notes |
|---|---|---|---|---|---|---|
| dm_turn_velocity | agent | SQL | message_map | no >30% 2-wk decline w/ 7/7 active days | W | **advisory class** (§3): organic decline (a tracked task completing) needs a human read — advisory alerts render and log but never auto-file backlog |
| owner_reply_latency_p50 | agent | SQL | message_map | stable/falling | W | **blocked**: `message_map` has one timestamp per exchange; bot→next-owner gap is confounded with the next turn's generation latency. Renders `not measured (blocked)` pending per-message timestamps (§6 prerequisites) |
| owner_verify_rate + reask_rate | agent | RX + SQL (char-n-gram near-dup <24h) | message_map | falling | M | **DM-scoped** until group sender attribution exists; falling = delegation trust |
| cost_per_chat_week | harness | SQL (usage spend grouped by chat_id and source) | usage | ≤ budget target | W | replaces cost_per_satisfied_turn: `usage ⨝ message_map` has no join key, so a per-turn cost÷satisfaction ratio is unimplementable; the ratio version is deferred behind a shared exchange-id harness change |
| proactive_cost_share | harness | SQL (heartbeat/scheduled sources' share of spend) | usage | tracked, baselined | W | **new** — proactive runs are 25–35% of all runs and previously appeared in no cost dimension |
| heartbeat_noop_rate | harness | SQL (heartbeat runs producing no owner-visible send) | usage + outbound | baselined | W | **new**; gated by outbound-ledger liveness |
| per_chat_liveness | harness | SQL (weeks-since-last-turn per known chat; **plus** flag any enabled schedule targeting a chat idle >2 wks) | message_map + schedules | flag >2 idle wks | W | catches silent stakeholder churn (owner-B DM) and proactive sends into dead chats |
| stale_answer_resend | agent | SQL (bot response char-n-gram near-duplicating its own earlier response in the same chat within 45 days; recurring scheduled content excluded) | message_map | ≤1/mo | M | **new** — a steady ~2/mo complaint class sits below the sampled judge tier's detection floor; this catches it deterministically |
| heartbeat_signal_rate | agent | SQL (owner reply ≤60 min after heartbeat send, fuzzy window vs outbound) | usage + map | provisional — set after baseline | W | goal deliberately unset until the backfill baselines it; gated by outbound liveness |

*Deferred out of v2 (see §6):* session_depth_avg (timestamp smearing + low marginal value), functional_ask_share.

### New — ledger integrity (ledger-deep + eval-gap-critic)

| dimension | layer | detector | source | goal | window | gameability / notes |
|---|---|---|---|---|---|---|
| ledger_liveness | harness | SQL — **per-signal**: each gated dimension declares its required columns and a populated/nonzero share threshold (e.g., `duration_ms>0` in ≥50% of rows), evaluated per feature flag | all ledgers | all declared signals live | W | **the anti-gaming keystone**: 0/0 now fails, not passes; per-table row counts were insufficient — a table can be full while its load-bearing column is dead |
| verifier_class_entropy | harness | SQL (any single class >95% over 30d → flag) | write/recall ledgers | 0 flags | M | catches miswired classifiers reporting perfection |
| tool_failure_rate | harness | SQL (per-tool failure share, all tools, per-class breakdown) | tool_uses | no tool > baseline+50% | W | **new** — general tool failures (shell/edit-class tools run ~8–23% failure) were unmeasured; SQL-trivial |
| partial_write_failure_rate | agent | SQL (`write_ok=1 AND write_failed=1`) | write_verifications | <3% | W | — |
| post_verified_correction_rate | agent | RX+SQL (correction shape ≤30 min after verified write) | map ⨝ ledger | ≤ baseline correction rate | W | generalizes to per-ledger claim-vs-transcript audit |
| inject_irrelevant_share | memory | SQL | recall ledger | <15% | W | — |
| memory_active_read_rate | memory | SQL (active memory-read tool calls per week) | tool_uses | no sustained collapse vs baseline | W | **new** — recall_grounded's rising goal is satisfiable by passive injection alone; this catches the drift toward passivity |
| memory_tool_failure_rate | memory | SQL (memory-tool failures) | tool_uses | <3% | W | — |
| tier_calibration_ratio + tier_discrimination | harness | SQL (realized deep÷everyday effort; majority-class share) | tier_decisions+usage | ≥2.0; <90% | W | currently degenerate (100% one class) — blocks live routing sign-off |
| model_attribution_coverage | harness | SQL (non-empty model share) | usage | >95% | W | currently 20–34%; prerequisite for any cost-mix claim |
| cross_ledger_coverage | harness | SQL (tier_decisions ÷ usage per source) | shell.db | >90% interactive | W | — |
| schedule_hygiene | harness | SQL (dup rows; enabled rows >2x cadence stale) | schedules | 0 | W | — |
| question_resends | harness/agent | SQL (char-n-gram near-identical adjacent owner rows <5 min) | message_map | <2/mo | M | strongest owner-not-served signal in the data; window corrected to match the monthly goal |
| dropped_reply_repings | harness | RX (delivery-failure re-ping shape ≤30 min after a prior owner turn) | message_map | 0/mo | M | delivery-path failure, distinct from nudges |
| relay_health + relay_scope | harness | SQL (relay failure rate; destination ≠ current-chat env to a human DM) | tool_uses | <5%; 0 unexpected | W | privacy surface |
| dup_reminder | harness | SQL (same-day fuzzy-dup proactive sends) | usage+outbound | 0 | W | — |
| script_violations | agent | RX (charset scan for disallowed script variant, **excluding messages with detected Japanese context** — kana presence or Japanese-lexicon match) | message_map | 0 confirmed | W | **demoted from hard-zero**: household chat legitimately contains Japanese, whose kanji share codepoints with the disallowed variant; raw hits go to a review queue as **advisory**, excluded from §3 rule 4 |
| intent_misreads | agent | RX (misunderstanding-shape lexicon on next owner turn) | message_map | ≤1/mo | M | — |

*Deferred out of v2.0 (see §6):* a2a_completion (feature not confirmed live; shared-store schema unverified; when shipped, the detector must treat the noop terminator as valid completion, not abandonment), history_denials (FTS keyword co-occurrence proves the wrong thing — FP on co-occurrence, FN on paraphrase — and its judge-review queue silently expands the judge tier; if revived, its flags are counted inside the §4 budget cap).

### LLM-batch tier (§4)

| dimension | layer | goal | window |
|---|---|---|---|
| answer_addressed_rate | agent | ≥97% | W (sampled ~50 pairs; rare failure classes below this floor are covered by deterministic dims, e.g., stale_answer_resend) |
| multi_turn_completion | agent | ≥90% confirmed-done | W (task-shaped threads) |
| promise_follow_through (topic match) | agent | ≥95% closed ≤72h | W (regex-extracted candidates; sampler re-inspects prior week's still-open candidates once — bounded 1-week lookback, older open candidates scored expired — so the 72h window can straddle the batch boundary without unbounded cost growth) |
| cross_agent_contradiction (full) | agent | 0/mo | M — **blocked** on group sender attribution (§6); interim coverage via the deterministic proxy; requires explicit owner sign-off before any cross-agent judge batch runs (§4 privacy) |

---

## 3. Trend Engine

**Snapshots.** Every eval run writes `~/.shell/eval/snapshots/<agent>/<ISO-week>.json` — the full `OwnerEvalReport` plus schema version and **per-dimension detector versions**: a hash of (detector code + that dimension's lexicon files). A whole-binary git SHA is too coarse — it makes every comparison warn forever after any change; per-dimension hashes localize invalidation. XDB dimensions are snapshotted once into a shared `~/.shell/eval/snapshots/pair/` namespace (not per-agent) so compare neither double-counts nor omits them. Backfill-generated snapshots carry `backfilled: true` — they were computed by current detectors over historical data and must not masquerade as contemporaneous readings. Private, gitignored territory; never enters the repo. Retention: keep all weekly snapshots (~KB); monthly rollups computed on read, not stored.

**Recompute on detector change.** Lexicon refreshes (e.g., internal_leaks quarterly) would otherwise sever exactly the month-scale trends v2 exists for. `shell ownereval backfill --dimension <name>` recomputes that dimension's historical snapshot entries under the current detector, restamping its per-dimension hash. Compare warns on hash mismatch only for dimensions actually mismatched, and the warning names the recompute command.

**Partial periods.** Monthly comparisons involving an incomplete current month compare *rates pro-rated by elapsed days*, never raw counts, and label the period `(partial)`.

**Compare.** `shell ownereval compare [--weeks 4|--months 3]` renders, per dimension: current value, `n/N`, period-over-period delta, sparkline direction, and goal status. Percentages are **suppressed below min-n** (default N≥20 denominators; dimension-overridable) and render as `3/11 (n too small)`. Dimensions whose data source or prerequisite is absent render **`not measured`** / **`not measured (blocked: <prereq>)`** — never 0, never goal-met. Low-n count dimensions (e.g., cross_agent_flight) render raw counts, not rates.

**Severity classes.** Two output classes, stamped on every dimension:
- **alert** — eligible to auto-file backlog items;
- **advisory** — rendered in compare and logged to `alerts.jsonl` with `severity:"advisory"`, but **never** auto-files backlog. Advisory members: dm_turn_velocity, script_violations, any judge dimension whose calibration agreement drops below 80% (§4).

**Regression alerts → backlog.** The evolve loop auto-files a backlog item when any of:
1. An alert-class dimension crosses from goal-met to goal-missed and stays there **2 consecutive periods** (weeks for W dims, **months for M dims** — monthly dimensions previously had no rule that could ever fire), with **n≥min in both periods**;
2. Period-over-period movement >30% **in the adverse direction for that dimension** (covers lower-better degradations *and* higher-better collapses, e.g., recall_grounded or attribution coverage falling) with n≥min in both periods;
3. `ledger_liveness` or `verifier_class_entropy` flags (immediate, no debounce — these mean the eval itself is lying);
4. Any **alert-class** hard-zero dimension (relay_scope, dropped_reply_repings, hop-cap breaches once a2a ships) goes nonzero. script_violations is advisory and excluded.

**Redaction of the alert pipeline (mandatory).** `alerts.jsonl` lives under `~/.shell/eval/` (private) and may carry row ids for local drill-down. Backlog items seeded into the repo-tracked backlog contain **only**: dimension name, numeric delta, period identifiers, and hashed chat ids. **No message text, no row content, no row ids, no per-owner personal detail ever crosses from `~/.shell/eval/` into a repo-tracked file.** The history of both public repos has already been scrubbed once; this pipeline must be structurally incapable of recreating that incident. Borrowed from Hermes's benchmark-gate pattern: hold-within-threshold as an explicit, mechanical gate.

---

## 4. LLM-Judge Tier

Per house constraint V2-H1 (keyword beat per-turn-LLM in this deployment), the judge tier is **batch, weekly, sampled, budget-capped, and optional**:

- **Trigger:** weekly cron or `shell ownereval judge`, never per-turn, never inline with the deterministic run.
- **Sampling:** systematic id-stride over the week's rows: ~50 question-shaped pairs (answer_addressed), all task-shaped threads up to 10 (multi_turn_completion), regex-prefiltered candidates only for promise_follow_through (including the bounded prior-week re-inspection) and contradiction — the layered-grader pattern: deterministic filters first, judge only on survivors. Any future dimension routing flags to judge review (e.g., a revived history_denials) counts inside this same cap — the tier's scope is the budget, not a fixed dimension list.
- **Budget cap:** hard per-run token/cost ceiling from config (suggested: small enough to be ignorable weekly). Cap hit mid-run → score what was judged, record actual n, stop.
- **Model:** cheapest adequate tier, tight rubric, 3-way labels (yes/no/unclear); unclear excluded from the denominator, counted separately.
- **Privacy:** judging sends transcript text only to the same provider account that generated the replies — the provider already saw this text at generation time; judging adds cost, not new exposure. **Exception:** cross_agent_contradiction batches both agents' transcripts into one judge context, crossing the deliberate agent-isolation boundary. That dimension does not run until the owner explicitly signs off on cross-agent batching; the config flag defaults off. No third-party eval platform, ever.
- **Graceful degradation:** no API key, budget zero, or run skipped → dimensions render `not measured (judge skipped)` with the last-judged week noted. **Never** carry forward stale numbers as current, never impute.
- **Calibration:** once a quarter, hand-label 20 judged pairs and record agreement in the snapshot; drop the dimension to advisory if agreement <80%.

---

## 5. Peer-Design Borrowings and Rejections

**Adopted:**
1. **Behavioral CSAT from implicit signals** (field survey: Nebuly/RLUF pattern) — abandonment, re-asks, verify-rate, and two-sided sentiment (gratitude tracked alongside complaints). The core of v2's agent layer.
2. **Layered graders** (OpenAI guidance) — deterministic filters gate every judge call.
3. **Tool-boundary evals over reasoning evals** (Anthropic guidance) — the harness layer scores ledgers, relays, schedules, delivery; not answer-intelligence.
4. **Regression gates with mechanical thresholds + human-review handoff** (Hermes self-evolution benchmark-gate pattern) — §3's alert rules; the loop files backlog items, a human approves fixes; the advisory class routes human-judgment cases away from auto-filing.
5. **Failure taxonomy as a first-class signal** (OpenHuman classified tool failures) — tool_failure_rate (all tools), memory_tool_failure_rate, relay_health, per-class breakdowns.
6. **Usage-telemetry lifecycle honesty** (Hermes Curator) — inverted into per-signal `ledger_liveness`: telemetry that stops flowing — at the column level, not just the table level — is a failure, not a pass.
7. **Bad turn → regression case** (Braintrust workflow, pattern only) — deferred hook: alert stanzas carry row ids *inside private `~/.shell/eval/` only* so a future golden-set builder can lift real failures into fixtures. Lifted fixtures are **repo-forbidden**: they live in the same private gitignored space as lexicons, never in the public repo.
8. **Sample real conversations, baseline, then threshold** (OpenAI) — v2 baselines are the March–July backfill, not aspirational numbers; every goal in §2 is provisional until its baseline pass.

**Rejected:**
1. **Hosted trace platforms** (LangSmith, Braintrust as products) — egress-incompatible; transcripts never leave the machine.
2. **Per-turn LLM judging** — already disproven locally (V2-H1); cost and latency unjustified for 2 owners.
3. **DeepEval as a component** — Python dependency in a Go deployment for what is ~200 lines of batch-judge code; adopt its *shape*, not the library.
4. **Golden-transcript replay suite** (field survey, claweval scenarios) — deferred, not rejected outright: replay burns real tokens, grading is nondeterministic, and v2's observational data covers the current failure classes. Revisit at v3 as a ≤10-golden PR-triggered suite once (repo-forbidden, privately stored) alert-to-fixture lifting exists.
5. **Bare sarcasm/tone regex** — spot-check showed heavy false positives on casual acknowledgment particles; only the clean resignation-marker subset ships. The mid-clause truncation heuristic was cut for the same reason.
6. **Self-evaluation by the agent** — the OpenClaw community's loudest complaint is that agent self-report is untrustworthy; every v2 detector reads ground truth (transcripts, ledgers), never agent claims — and post_verified_correction_rate exists precisely because even the *verified* claims lie.
7. **OpenClaw-style ops monitoring as a substitute for evals** (ClawMonitor) — stall detection is useful but orthogonal; we take only the dropped-reply insight (generated ≠ delivered) as a harness dimension.

The absence finding stands on its own: no surveyed peer measures whether a deployed personal agent is getting better or worse for its actual owners over time. v2 is that instrument.

---

## 6. Implementation Plan

Scope is cut for a solo maintainer: v2.0 ships only single-DB deterministic dimensions with no fragile joins, and nothing blocked on harness changes.

**Phase v2.0 — deterministic core (first PR series).**
- Extend `Dimension` (backward-compatible additions): `Detector string` (`"regex"|"sql"|"xdb"|"llm-batch"`), `Window string` (`"week"|"month"`), `MinN int64`, `Measured bool`, `BlockedOn string`, `Severity string` (`"alert"|"advisory"`), `DetectorHash string`, keep `Layer` now tri-valued (`harness|agent|memory`).
- `internal/bench/ownereval.go` — orchestration, struct, formatting (n/N rendering, min-n suppression, `not measured` / `blocked` states, pro-rated partial periods).
- `internal/bench/dims_map.go` — the clean RX/SQL set over `message_map` only: factual_corrections, complaint_rate, gratitude_marker_rate, resigned_marker_rate, nudges_unanswered, verbosity_casual, internal_leaks, intent_misreads, reply_truncation (two deterministic sub-signals), reminder_failure_complaints, question_resends, stale_answer_resend, dropped_reply_repings, script_violations (advisory), language_mirror_rate (non-prose stripping), instruction_reissue. Lexicons from `~/.shell/eval-lexicons/`; English-only textbook fallbacks; CJK-dependent dims without private lexicon render `not measured`.
- `internal/bench/dims_ledger.go` — ledger-integrity family: per-signal ledger_liveness, verifier_class_entropy, tool_failure_rate, partial_write_failure_rate, post_verified_correction_rate, inject_irrelevant_share, memory_active_read_rate, memory_tool_failure_rate, tier dims, attribution/coverage, schedule_hygiene, relay dims, dup_reminder, cost_per_chat_week, proactive_cost_share, per_chat_liveness (with idle-schedule flag).
- **Baseline pass:** run the March–July backfill, derive/confirm every threshold and similarity cutoff; goals are report-only until this pass completes.

**Phase v2.1 — trend engine + engagement + cross-DB.**
- `internal/bench/snapshot.go` — write/read `~/.shell/eval/snapshots/` (+ `pair/` namespace for XDB dims), schema versioning, per-dimension detector hashes, `backfilled` flag.
- `internal/bench/compare.go` + `shell ownereval compare` — deltas, sparklines, severity-aware alert evaluation (weekly and monthly rules), `alerts.jsonl` emission, `shell ownereval backfill --dimension` recompute path.
- Loop integration: charter step reads alerts and seeds backlog items under the §3 redaction mandate (dimension name + delta + period + hashed chat id only).
- `internal/bench/dims_engagement.go` — dm_turn_velocity (advisory), owner_verify_rate/reask_rate (DM-scoped), heartbeat_noop_rate and heartbeat_signal_rate (gated), thread_abandonment_after_dissat.
- `internal/bench/crossdb.go` — `ATTACH DATABASE` second agent DB (modernc supports it); cross_agent_flight (DM-only), cross_agent_contradiction_proxy (group-row dedup by text hash). Degrades to `not measured` if peer DB absent.

**Phase v2.2 — judge tier.**
- `internal/bench/judge.go` — sampler (with bounded prior-week promise re-inspection), rubric prompts, budget cap, HTTP API (not CLI subprocess, per the router lesson), graceful-skip, cross-agent sign-off gate. Config block in `~/.shell/config.json` (`eval.judge.enabled/budget/model/crossAgentApproved`).

**Deferred / blocked (explicitly not in v2.0–v2.2 unless prerequisites land):** owner_reply_latency_p50, session_depth_avg, functional_ask_share, schedule_fire_miss, a2a_completion, history_denials, cross_agent_contradiction (full), per-turn cost÷satisfaction ratio.

**Harness prerequisites (file as backlog items alongside v2.0; each blocked dimension names its prerequisite in `BlockedOn`):**
1. Populate `usage.model` on long-lived sessions (attribution at 20–34% blocks cost-mix claims).
2. Sender attribution on `message_map` group rows (blocks group-scoped owner dims and full contradiction).
3. Shared exchange id across `usage` ↔ `message_map` (blocks per-turn cost ratio).
4. Per-message timestamps (owner-sent vs bot-sent) in `message_map` (blocks reply latency and precise segmentation).
5. `outbound_sends`: retain platform message id, and verify/implement schedule-id stamping in `source` (blocks exact send joins and schedule_fire_miss).
6. Populate `duration_ms`/`queue_ms` (blocks latency_p95 per-signal liveness).
7. Investigate the degenerate one-class tier router before any live-routing sign-off.

**v1 baseline migration.** v1 snapshots (if any) are kept but marked schema-v1. On first v2 run, a one-time backfill replays all month-window dimensions over `message_map` from March, writing retroactive monthly snapshots flagged `backfilled: true` and stamped with current per-dimension detector hashes. v1 dimensions keep their names where semantics are unchanged (`write_confabulation`, `recall_grounded`); the six transcript dims get a `_map` suffix during one release of parallel operation, then the old readings are retired. Dead-ledger v1 dims are retroactively re-rendered as `not measured` in historical views — no rewriting of numbers, only of their goal-met status.

**Definition of done for v2.0:** `shell ownereval` renders all v2.0 deterministic dimensions for both agents with n/N, three layers separable, zero LLM calls, all thresholds baselined against the backfill, and at least one full weekly snapshot written and comparable.

---

## Appendix — Rejected Critiques

- **Feasibility #14 (partial) — cut `dm_turn_velocity` to a report:** rejected; kept as an **advisory-class** dimension. The critique's actual risk (organic decline auto-filing backlog noise) is eliminated by the advisory severity class (completeness #11), and the detector is trivial SQL — removing it would blind the trend engine to the engagement-depth story that motivated v2.
- **Completeness #2 (partial) — the implication that complaint-class dimensions need weekly-window rates to catch sub-month peaks:** rejected as stated; weekly *rates* on owner-behavior dims would be chronically min-n-suppressed (feasibility #15). Adopted instead: weekly raw counts recorded in every snapshot plus the new month-over-month alert rules, which together surface both the mid-June weekly peak and the MoM regressions.

All other critique points from both reviews were accepted and applied above.

*Approx. 3,400 words.*