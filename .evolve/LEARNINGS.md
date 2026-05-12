# Learnings (append-only, chronological)

Each entry is dated and tagged. Newest at bottom.

---

## 2026-05-07 — cycle 0

- **Skill telemetry not landing.** No `USAGE.jsonl` exists anywhere under `~/.shell/skills/` or per-agent skills dirs, despite the architecture doc describing it. Either logging is unimplemented in `scripts/run-skill`, or the path is elsewhere. We are flying blind on skill failure/latency.
- **`[noop]` heartbeat suppression has fired 0 times** in pikamini's transcripts — confirms ghost-memory note from 2026-04-15. The signal is too restrictive or the prompt never invites it.
- **Legacy `[relay to=...]` text directive has fired 0 times** — confirms it's fully deprecated. Cleanup candidate.
- **Pikamini's transcripts go to shared DB; umbreon's to local DB.** Topology mismatch with new design rule "each agent owns its transcript." Pikamini is the one out of place, not umbreon.
- **Pikamini self-recovery on 2026-05-06 21:00–21:47** — agent caught its own hallucinated reminder ("Need to verify the 7:20 AM reminder was actually scheduled") and switched from remote `/schedule` to `shell-schedule`. This is exactly the kind of behavior worth codifying as a convention memory.
- **Umbreon ghost: 109 ltm vs 759 dormant** — unusually aggressive decay vs. pikamini (2517 ltm vs 1085 dormant). Either umbreon's reflect rules are tuned differently or the smaller corpus is decaying faster than it accrues.
- **Personality split is real** — pikamini avg msg length 458 chars (functional), umbreon 257 chars (companionship). Confirms organic divergence; backs the case for per-agent skill differentiation.
- **No agent has authored a skill yet.** Both per-agent `skills/` dirs contain only the inherited `run-skill` shim. The loop's primary mandate.

## 2026-05-07 — cycle 1

- **Reflect-rule asymmetry diagnosed.** Umbreon's `sys-promote-to-ltm` requires `access>50` vs pikamini's `access>3`; decay kicks in at 48h vs 72h. Bootstrap defaults shifted between 2026-03-08 (pikamini) and 2026-03-23 (umbreon). This fully explains the 109 ltm vs 759 dormant ratio. Concrete fix landed in B-006.
- **`sys-pin-identity` rule missing from umbreon's DB.** Pikamini has a priority-1 rule that protects identity-tier memories from decay; umbreon does not. Personality drift exposure.
- **`reflect_rules.created_by` defaults to `system`** — schema already supports per-agent custom rules. Future hook: agents authoring their own decay/promote rules scoped to their namespace. Worth a future backlog item.
- **Quiet hour:** only 1 new transcript message in the cycle window (papi camping, mami quiet morning).

## 2026-05-07 — cycle 2

- **Pikamini and umbreon's voices are operationally distinct, not stylistic variants.** In the last 200 msgs each: pikamini uses 248 bulleted lines and 42 dairy totals; umbreon uses 0 bullets and 115 halo openers. Almost no overlap in topic vocabulary. Confirms the case for completely separate per-agent skill libraries.
- **~30% of pikamini's mami DMs are meal memos.** Dominant skill candidate. Format already stable.
- **Umbreon has near-zero functional vocabulary.** 5 mentions of 提醒 across 200 msgs (vs pikamini's daily reminder activity). Companion role is fully realized; functional ask should always hand off to pikamini.
- **Drafted first two per-agent skills** (`meal-memo` for pikamini, `gentle-checkin` for umbreon) — both await B-007 approval before install.

## 2026-05-07 — cycle 3 (first ships)

- **B-005, B-007, B-009 shipped.** Verify-after-schedule convention written to pikamini's ghost (id `01KR1Q3K2PNTDCJTHSHGEB1KFQ`); both per-agent skill files installed; noop investigation closed.
- **The "noop never fires" learning was wrong** — it does fire. Our cycle-0 metric was structurally broken: `text LIKE '%[noop]%'` against `transcript-v2.db` can't match because `stripDirectives` removes the marker before transcript logging. **Lesson: always sanity-check measurement code paths before drawing behavioral conclusions.**
- **Skill install is not "live install."** Skills load into the system prompt at session start. Dropping a `SKILL.md` into `~/.shell/agents/<n>/skills/` only takes effect on the next rotation or restart. The architecture doc was right; I just hadn't internalized it. Future skill-install cycles need a "wait for rotation OR coordinate restart" step.
- **Restart is risky when main has WIP.** Pid 2615/2616 are running pikamini and umbreonmini daemons; main has 10 modified bridge files. The right pattern is to never auto-restart with uncommitted bridge code — surface to user instead.

## 2026-05-07 — cycle 4 (real telemetry)

- **Real heartbeat noop rates: pikamini 56%, umbreon 29%.** Measured via per-agent `shell.db` empty-assistant rows. Heartbeat suppression works correctly; pikamini saves ~half its heartbeat output, umbreon less than a third. Cost-optimization angle for EV.
- **Heartbeat fires can be timestamped from the noop pattern** — empty rows cluster at xx:01 each hour. Useful lightweight signal for future cycles.
- **Group-chat noop ~0.4% on both agents.** Hypothesis "umbreon never speaks in group" needs a different framing — it's not noop suppression, it's the cross-agent visibility issue (each agent writes to its own DB; mutual awareness would need a shared read path).
- **Mami DM noop = 0% on both.** Agents are reliable correspondents to mami.
- **Per-chat ascertainment of noop rate is now a cheap signal** — no instrumentation needed, just `shell.db` queries. Future cycles can include it as a one-liner.

## 2026-05-07 — cycle 5 (ghost cluster quality + bug found)

- **🚨 Ghost `access_count` is inflating pathologically.** Umbreon's top entry has access_count = 10.6 trillion (literally a nanosecond timestamp). Pikamini has multiple memories with counts in the millions despite being weeks old. Strongly suggests an increment-by-timestamp bug somewhere in the access path. Filed as B-012.
- **`sys-prune-low-utility` rule is a no-op for the worst offenders** because they're already dormant. We've accumulated a layer of "stuck dormant with inflated counts" memories that no rule reaches. Need a separate cleanup rule (B-013) for `auto-summary-*` / `exchange-summary-*`.
- **Pikamini's edge graph is weak (avg weight 0.22) vs umbreon (0.59).** Many low-weight auto-similarity edges accumulating without pruning. Either tune `GHOST_EDGE_MIN_WEIGHT` or delete weak edges (B-014).
- **Noise ratio (access>5, importance<0.4): 17–18% on both agents.** Roughly consistent across very different corpus sizes — likely a function of how memories are written, not how they age. Worth keeping as a baseline metric to track over time.

## 2026-05-07 — cycle 6 (live activity)

- **First observed cross-agent fact-check.** Umbreon conceded openly to pikamini's correction (*"皮卡講的對... fact check 失敗 — sorry"*). Healthy — agents are aware of each other and willing to defer publicly. One data point; codify after more samples.
- **Confirmed umbreon over-responds on heartbeat.** Caught a live example: chat_id=0 heartbeat sent *"Check-in sent to mami. Nothing else needs attention this tick."* — this is the noop that wasn't. Reinforces B-011 hypothesis and B-006 fix priority.
- **Both agents are confused about ghost vs MEMORY.md.** Mami had to push umbreon to also save plant records to ghost (initially only local). Pikamini explained the distinction correctly. The decision tree isn't internalized. New B-016.
- **Skill activation latency confirmed** — B-007 drafts installed yesterday, agents still on old prompt. Until restart, the loop's skill-install channel won't show measurable behavior change. Need to plan around this, not work around it.

## 2026-05-07 — cycle 7 (heartbeat prompt diff)

- **The heartbeat prompt is byte-identical between agents.** Both configs have `system_prompt: ""`. The 56%/29% noop gap is entirely emergent from ghost identity context (and recent transcripts). Confirms that B-006 (fix umbreon reflect rules) is the single highest-leverage ghost fix.
- **Heartbeat prompt has an internal tension** — line 203 says "send a brief check-in" while line 206 says "[noop] if nothing to do." Pikamini resolves toward noop; umbreon resolves toward sending a check-in. New B-017 to resolve.
- **Declared `agent.skills` is for delegation routing only.** Pikamini advertises {coding, research, web-search, image-generation, planning, tool-use}; umbreon advertises {emotional-support, verification, code-review, daily-planning, companionship}. These are nominal labels for cross-agent task assignment; they don't load actual SKILL.md.

## 2026-05-07 — cycle 8 (baseline capture)

- **Umbreon's heartbeat noop rate is drifting DOWN** (29% → 27% in 4h). Without a fix it gets worse, not stays-the-same. Confirms the right next move is to ship B-006 + B-017 rather than wait.
- **Pikamini noop rate is rock-stable** (~55%) — its identity context is well-anchored.
- **Cycle-to-cycle deltas establish a measurable trend.** Capturing baseline metrics before shipping behavior fixes is now a standard part of the cycle template.
- **Session-rotation prunes old `shell.db` rows.** Raw counts can decrease over time; ratios are the trustworthy trend signal.

## 2026-05-07 — cycle 9 (agent's own reflection)

- **Pikamini runs its own deep-reflection heartbeat** that does response-quality audit, pattern detection, skill inventory, and behavioral memory pinning. Significant overlap with our evolve loop.
- **Pikamini independently arrived at the B-005 finding** — it pinned `behavioral-heartbeat-check-ghost-first` itself. The convention work is converging, just from two directions.
- **The loop's right scope is the layer the agent can't reach** — shell/ghost code, config, prompts, skill files. Behavioral memory pinning is the agent's job. Don't duplicate.
- **Pikamini self-reports "All 10 skills lazy/0 runs"** — independently confirms B-001 telemetry gap. The agent notices what the loop notices.
- **Cadence doubled to 2h while idle.** 9 hours of unattended observation costs more than it returns until approvals land. Easy to revert.

## 2026-05-07 — cycle 16 (substantive ships)

- **B-006 + B-012 shipped together.** Umbreon reflect rules harmonized, ghost access_count SUM→MAX bug fixed in code (`evolve/c16-access-count-max` branch, commit `89d891b`), 31 inflated rows clamped to 10000 across both DBs.
- **The bug was real and fixable in one focused commit** — 21 lines changed in `internal/store/reflect_merge.go`. Root cause was straightforward once the data anomaly was surfaced. The investigation phases (cycles 0–7) earned their cost by producing this concrete fix.
- **Loop meta-improvements adopted:** auto-stop after 4 idle cycles, quiet-hours alignment, per-cycle cost tracking (placeholder), top-3 approval surface. README updated.
- **Bridge WIP continues to gate shell-side ships.** B-017 ready in spirit but not actionable. The "soft-reload skills" idea (proposed in retro) would unblock similar ships without coordinating with bridge editors.

## 2026-05-08 — cycle 21 (loop redesign)

- **Loop now ships low-risk autonomously.** Two-tier approval: low-risk auto-ships (ghost puts, prompt-only edits, additive reflect rules); high-risk gated (DB cleanups, mass mutations, identity memory deletes). User explicit instruction: "you can continue to push forward without needing approval for everything."
- **Reflect & consolidate is now standard.** Backlog scan happens before adding new findings. This cycle closed B-002, B-010, B-011 as superseded.
- **Agent self-reflections are now the primary cycle input.** Cycle 9's finding (pikamini already does deep self-reflection) is now structural, not just observational.
- **`validating` status added.** Every shipped item carries predicted-effect + measure-by. Loop reads its own track record over time.
- **Deep-reflect was already on for both agents.** Investigation found `defaultDeepReflectInterval=6`. Pikamini and umbreon both fire deep heartbeats at the same rate; the asymmetry from yesterday was reflect-rule quality (B-006, fixed), not the deep-mode trigger.
- **Splitting items by risk works.** B-013 split into B-013a (low-risk rule shipped) + B-019 (high-risk mass-tag, gated). Lets us advance infrastructure without waiting for full approval.

## 2026-05-08 — cycle 23-28 (loop running under new design)

- **B-020 ship validated the new design** — caught mami's repeat correction about Telegram tables organically and shipped autonomously within 1 cycle. First end-to-end proof that auto-ship + transcript signal detection works.
- **B-006 graded as wrong-lever** at 30h. Both agents' heartbeat noop rates trended DOWN, not up. The asymmetry diagnosis from cycle 1 was real (umbreon's rules ARE less protective) but didn't translate to noop-rate movement. Lesson: structural correctness fix ≠ behavior change. The right next test is B-017 (heartbeat prompt). Don't revert B-006 — it still fixed identity-tier protection.
- **Track-record builds discipline.** Predicted "umbreon noop rises to 40%+" → actual "dropped to 27%". The validating step caught the wrong hypothesis. Without it, we'd have happily moved on assuming success.
- **Pikamini's ghost ns is contaminated by Claude Code dev-session summaries** (B-021). 730 `session-*` rows. Auto-memory hook routes by cwd, and cwd `~/src/pikamini/...` becomes `agent:pikamini` regardless of who's actually working. Significant retrieval pollution; high-risk to fix.
- **Idle cycles cost ~thousands of tokens each** even at lightweight. Loop's "find new signal" rate is low — most wakes have 0 corrections, 0 new behavioral memories worth shipping. The economy improves only when a real signal lands (B-020 did). Need a better idle/signal ratio.
