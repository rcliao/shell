# Design: goal schedules and the project steward

Status: draft v2, 2026-10-13; updated 2026-10-14. Step 0 shipped (PR #81). Step 1 shipped in shadow mode (PR #82). After one day of shadow proposals the owner asked for shorter ones, answered per project by the people of that project, so `on` mode now posts each proposal in its project's thread. Steps 2 (goal schedules, project goals only) and 3 (DoneJudge) are built. Step 4 (LessonStore) is built, recalled by project only: goals have no kind yet. Steps 5–6 are not built.
v1 proposed a separate `loops` table. The owner redirected: extend the existing heartbeat/cron schedules instead, and let the agent create loops from a project's context.

## 1. Intent
Agent moves projects forward.

## 2. Decisions
- **A goal on the schedule row, over a new loops table** — schedules already fire, retry, keep a run ledger, and are created by the agents themselves. A goal plus a done check turns any of them into a loop[^E1].
- **Loops are born from projects, over waiting for a request** — a steward pass reads each project's 目標, 現況 and 待決定 and proposes the next goal. Today a project has no done and nothing moves it except research[^E2].
- **The agent claims done; a judge confirms it** — a separate model call defaults to "not done". Self-graders praise their own work, and the one gate we have defaults to done[^E3].
- **Shadow first: proposals go to the owner, not the family** — the owner rates them for a week before the agent asks the family directly[^E4].
- **Lessons are recalled by project and goal kind, over by the latest message** — today's recall is keyed on `userMsg`, and nothing fetches by task[^E5].

## 3. Components
- **ProjectSteward** — a deep-heartbeat move that proposes next goals for active projects
- **SuggestionQueue** — the existing suggestions table; holds proposals until decided
- **GoalSchedule** — a schedule row carrying goal, criteria, notes and project
- **Scheduler** — fires goal schedules; project ones run in the project's thread
- **DoneJudge** — rules done, continue or impossible from the agent's evidence
- **LessonStore** — ghost lessons written on close, recalled on the next fire
- **WeeklyReview** — reports steward acceptance and goal outcomes

## 4. Data flow
1. On a deep beat, **ProjectSteward** reads the active projects (目標, top 待決定 lines, last activity, schedules) and picks the one or two that most need moving.
2. **ProjectSteward** files up to 2 proposals in **SuggestionQueue** (source `steward`, audience owner). Each is one of: "I'll do X now", "ask the family Y", or "goal schedule: goal, done criteria, cadence".
3. The daemon sends new steward proposals to the owner's DM after the beat (shadow mode) rather than waiting for the weekly review.
4. The owner answers in plain words. On a yes to a goal, the agent creates a **GoalSchedule** with `shell_schedule(create, goal, done_criteria, project, max_runs)`.
5. **Scheduler** fires the **GoalSchedule**. A project goal runs as a project turn in that project's thread[^E6]. The turn gets the goal, notes, the last verdict and what **LessonStore** recalls for the project and goal kind.
6. The agent does one step and calls `shell_schedule(progress)` with evidence and new notes, optionally setting the next run. Or it calls `shell_schedule(done)` with evidence.
7. **DoneJudge** reads the goal, criteria and evidence. On done, the schedule is disabled with reason `done: …`, the verdict lands in that fire's `job_runs` row, one line goes to the thread, a 決定 entry goes into the doc, and **LessonStore** writes up to 3 lessons. On "not met", the reason goes back to the agent and the schedule keeps running.
8. **WeeklyReview** reports steward acceptance and goal outcomes, so the owner can decide when steward proposals may go to family threads directly.

## 5. Data model
| thing | why |
|---|---|
| `schedules` + `goal`, `done_criteria`, `notes`, `project_id`, `thread_id`, `max_runs`, `goal_status` (open/done/impossible/expired) | any heartbeat, cron or once schedule can carry a goal; notes replace the frozen state copied into each `once` prompt[^E7] |
| `job_runs` + `verdict`, `reason`, `evidence`, `judge_cost_usd` | one row per fire already exists, so it becomes the iteration record |
| `suggestions` rows with `source = 'steward'` | proposals reuse the queue and the owner's decide path; owner-audience rows are not touched by the chat retro's withdraw[^E8] |
| ghost memories tagged `lesson`, `project:<slug>`, `goal:<kind>` | lessons that reach the next fire for the same project or kind |
| config `scheduler.project_steward`: `off` / `shadow` / `on` | experiment switch; `on` lets the steward ask in the project's thread |

## 6. Interfaces
- `shell_schedule(create, …, goal, done_criteria, project?, max_runs?)` → id. **Errors:** a goal without criteria → refused; the project is not active → refused. Without `goal`, behaviour is unchanged.
- `shell_schedule(progress, id, evidence, notes, next_at?)` → ok. **Error:** the schedule has no goal → refused.
- `shell_schedule(done, id, evidence)` → `{verdict, reason}`. Not met → the reason comes back and the schedule keeps running (not an error). Empty evidence → not met.
- DoneJudge `(goal, criteria, evidence)` → `{verdict, reason, cost_usd}`, via `claude -p --output-format json` with Bash, Write and Edit disallowed and a scrubbed environment. **Errors:** timeout or unparseable output → not met; 3 failures in a row → an ask to the owner.
- `max_runs` reached → `goal_status = expired`, the schedule is disabled, and one line goes to the thread.
- Quiet hours apply to goal schedules, not only to heartbeats[^E6].

## 7. Plan
0. Heartbeat [CHANGE] — fix the 待決定 nudge that fires on every beat and the lesson prune that never caps[^E9]. Prerequisite: the steward reads the same project list.
1. **ProjectSteward** [NEW] — a deep-beat move and its prompt; files `steward` suggestions; the daemon sends them to the owner's DM; `shadow` mode. This is the experiment, and it needs no schema change.
2. **GoalSchedule** [CHANGE] — the new columns, the `shell_schedule` arguments, project goals fired through the project-turn path, and quiet hours.
3. **DoneJudge** [NEW] — wraps `route.ClaudeCLI` with JSON output; default not met; a `usage` row with source `judge`.
4. **LessonStore** [NEW] — write on judged close; recall by tag on each fire.
5. Pilot — Umbreon's 3 price watches become 3 goal schedules (7 `once` rows disabled).
6. **WeeklyReview** [CHANGE] — steward acceptance and goal outcomes; the owner flips `project_steward` to `on`.

## 8. Done
- The experiment produced rated proposals · proof: `sqlite3 ~/.shell/agents/<a>/shell.db "select status,count(*) from suggestions where source='steward' and created_at>date('now','-7 day') group by 1"` for both agents → rows exist and none are still `proposed` · invariant: no steward message went to a family chat while in shadow mode.
- A goal schedule closes only through the judge · proof: `go test ./internal/scheduler/... ./internal/rpc/... -run 'Goal|Judge'` → PASS, covering empty evidence → not met, unparseable → not met, met → schedule disabled with `done:` · invariant: the judge runs without Bash, Write or Edit.
- Lessons carry over · proof: `go test ./internal/... -run TestGoalLessonCarry` → PASS; the second fire's prompt contains the lesson key the first close wrote.
- The pilot is live · proof: `sqlite3 ~/.shell/agents/umbreonmini/shell.db "select count(*) from schedules where goal!='' and enabled=1"` → 3, and the 7 price-watch `once` rows are disabled.

---

## Evidence

[^E1]: `shell_schedule` (`internal/mcp/server.go:220`) already supports create, list, describe, cancel, trigger and queue, and the agents use it: Umbreon has 9 live `once` rows and Pika 2. Each fire is recorded in `job_runs` (`schedule_id, outcome, error_message, attempt, duration_ms`), and the scheduler retries through `RetryPolicy` (`internal/scheduler/policy.go:66`) and, since PR #74, after a preempt.

[^E2]: A project has no definition of done. Statuses are only `active|paused|archived` (`internal/store/projects.go:22,60`). 目標 is one prose line (`internal/project/doc.go:93-103`). The only recurring work is the weekly research pass (`internal/project/event.go:119-147`), which carries forward nothing but the doc. `review_after` is NULL on all 26 project rows. Open 待決定 counts per doc run up to 12.

[^E3]: Anthropic found self-evaluating agents "confidently prais[e] the work" (anthropic.com/engineering/harness-design-long-running-apps). METR saw reward hacking in 70–80% of runs even when told not to (metr.org/blog/2025-06-05-recent-reward-hacking). Claude Code's /goal judge "can only judge what Claude has already surfaced" (code.claude.com/docs/en/goal), so evidence must be in the call, as text. The only gate in shell, `internal/planner/planner.go:571`, reads "VERDICT: done — DEFAULT". `route.ClaudeCLI` (`internal/route/judge.go:102-115`) uses text output with no cost and sets no `Env` or `Dir`, so the judge wrapper needs JSON output and `ChildEnv` (`planner.go:67-71`).

[^E4]: Claude asks for clarification more than twice as often as humans interrupt it, and 35% of its asks are a choice between approaches (anthropic.com/research/measuring-agent-autonomy). Horvitz weighs "the costs of potentially bothering a user needlessly" (erichorvitz.com/uiact.htm). The owner decides suggestions in a median of 8.4 h for Pika but 176 h for Umbreon, because weekly delivery is capped at 3 (`internal/daemon/review.go:419`). That is why the steward sends to the DM right after the beat.

[^E5]: Per-turn recall is `ghost Context(Query=userMsg)`, 2/3 hard-filtered to `chat:<id>` (`internal/memory/memory.go:510-536`). No code fetches by `project:` or by task. Lessons are never pinned, and the pin budget already overflows ("pinned delta over budget" lines in daemon.log). No table records which memory keys were injected into a turn. The agent's own lesson-action on 10/10: "a rule stored as an unpinned ghost memory does not fire on its own."

[^E6]: The `schedules` table has `chat_id` but no thread. A prompt-mode schedule runs `syntheticTurn(ctx, br, chatID, 0, msg, "scheduler")` (`internal/daemon/daemon.go:939`), so it posts to the main thread. Project research is an event-mode schedule whose consumer runs `runProjectTurn` (`internal/daemon/projectevents.go:206`) → `syntheticTurn(…, chatID, threadID, …, ProjectTurnSender)` (`daemon.go:1186-1191`) and delivers to the project's thread. Project goals reuse that path. Quiet hours apply only to `type=heartbeat` (`internal/scheduler/scheduler.go:497-500`).

[^E7]: Umbreon's live `once` rows (2026-10-13): 9, of which 7 are 3 price watches (luggage ×3, Dyson ×2, C&K ×2). The luggage prompts each carry a frozen "status as of 2026-10-07". No row holds what an earlier check found.

[^E8]: The chat retro withdraws all but the newest *chat-audience* proposed suggestion (`internal/daemon/chat_retro.go:250-260`). Owner-audience rows are delivered by the weekly review, oldest first, at most 3 (`review.go:419`). The decide action works only in the owner's chat (`internal/rpc/suggestion.go:78-89`).

[^E9]: (a) Heartbeat agenda item 4 (`internal/bridge/agenda.go:146-164`) skips a project only when `LastHumanActivityAt` is recent. Only the Notion poll writes that field, and it is NULL on all 26 rows, so every active project with an open 待決定 is flagged on every beat. (b) Agent-wide heartbeat learnings carry only the `heartbeat` tag (`internal/memory/memory.go:1259-1268`), but the prune lists `{heartbeat, chat:0}` (`:1288-1296`), and ghost List requires every tag. So the cap of 20 never applies: 150 and 147 rows are live.

### Explored, not in the body
- **v1: a separate `loops` table plus an `asks` table on the task queue.** Dropped at the owner's direction; schedules plus `job_runs` cover the same ground.
- **Giving the steward the full doc of every project.** Too big: docs are up to 24 KB and Pika has 13 active projects and areas, Umbreon 8. The steward gets 目標, the top 待決定 lines and last activity, and reads at most 2 docs per beat (assumption: enough to pick well).
- **The steward on every hourly beat.** It runs on deep beats only (every 6th, already Opus) to keep cost flat (assumption: no measurement yet).
- **A judge that reads screenshots.** Deferred: v1 needs text evidence.
- **Costs.** 30-day Pika spend by source: interactive $500, prewarm $300, heartbeat $113, scheduler $74. A Sonnet judge call per `done` claim is estimated at a few cents (assumption).
