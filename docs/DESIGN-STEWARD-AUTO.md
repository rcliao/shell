# Steward auto: do low-risk steps, then report

Status: built 2026-10-17; deployed with `steward_auto: "shadow"`. Follows
DESIGN-STEWARD-EVAL.md. Out of scope: proceeding on silence, auto-creating
recurring goals, and anything that spends money or sends messages.

## 1. Intent
Do the safe step; report it.

## 2. Decisions
- **Do it now and report, not "yes unless you object".** A step safe
  enough to happen on silence is safe enough to do right away. For anything
  riskier, silence is not consent: 6 of the 7 in-thread proposals since
  10/15 got no answer.[^E1]
- **The steward picks auto; the RPC enforces the caps.** The prompt says
  what counts as low-risk. The RPC refuses auto when it is off, when the
  project already had one this week, or within 30 days of a "no" there.
  It cannot check what a turn does with its tools, so the turn's prompt
  repeats the rules and tells it to drop the step if it breaks one.
- **The step runs as a project turn, not in the beat.** Queued as a
  project event, it gets the thread's session, the 20-minute project-turn
  budget and queue retries, and the heartbeat is not held up.
- **Shadow first.** With `steward_auto: "shadow"`, the owner's DM says what
  would have run and the row is withdrawn. `"on"` runs it. This is the same
  rollout the steward had.

## 3. Components
- **StewardRenderer**: offers the auto kind and its rules when auto is on.
- **AutoGuard**: refuses auto proposals that break a cap (RPC).
- **StewardDelivery**: queues auto steps, or reports them to the owner in shadow.
- **AutoTurn**: does one auto step in the project's thread and reports it.
- **ReviewEvidence**: counts auto steps apart from the family's answers.
- **LowRiskJudge**: the eval's `low_risk` check on auto proposals.

## 4. Data flow
1. On a deep beat, StewardRenderer adds the auto kind and its rules to the steward block. This happens only when `scheduler.steward_auto` is "shadow" or "on".
2. The steward files `shell_suggestion(source="steward", evidence="AUTO. …", change=<the task>)`. AutoGuard checks it, and on a refusal the steward files a DO instead.
3. After the beat, StewardDelivery takes the row.
   - With auto "on" and the steward in "on" mode, it enqueues `project.event{steward.auto, suggestion_id}` and marks that row delivered right away. Nothing is posted yet, and thread turns do not list the row as an open proposal.
   - Otherwise it adds "would do without asking (auto, shadow)" to the owner's digest and withdraws the row by `shadow`.
4. AutoTurn leases the event and runs the step in the project's thread.
   - On a report, it posts the reply there (what it did, what the family gets, one yes/no next step) and closes the row accepted, then done, by `auto`.
   - On `[noop]`, it withdraws the row by `auto`.
   - On a failed turn, the queue retries. A row still waiting 25 h after delivery is withdrawn as "did not run" by the next delivery, so it stops holding its project.
   - If auto was turned off after the step was queued, the row is withdrawn and the step never runs.
5. ReviewEvidence lists auto rows as `[auto …]` and keeps them out of the answer rate. The steward's history lines say "you did it without asking and reported".

## 5. Data model
| thing | why |
|---|---|
| no schema change | the kind is the evidence's first word (`AUTO`), as DO/ASK/GOAL already are |
| `suggestions.decided_by` = `auto` / `shadow` | how an auto row ended: run by the daemon, or seen only by the owner |
| `tasks` row `project.event{steward.auto}` | one per proposal (idempotency key = its id), expires after 24 h |
| `scheduler.steward_auto` config | "", "off", "shadow" or "on" |

## 6. Interfaces
- `shell_suggestion` create with evidence starting `AUTO`:
  - 409 "auto is off…" when `steward_auto` is not shadow or on
  - 409 "…said no to proposal #N…" for 30 days after a declined steward proposal in the project
  - 409 "…already ran (or was shadowed)… this week" when an auto was filed for the project in the last 7 days
  - the existing per-beat and one-open-per-project caps still apply
  - each refusal tells the agent to file a DO instead
- `bridge.DefaultStewardOptions(mode, auto string)`: `Auto` is true for
  "shadow" or "on".
- `store.StewardKind(evidence) → do|ask|goal|auto`, with "ask" when there is no kind word.
- `project.EnqueueStewardAuto(q, p, id, now)` and
  `project.StewardAutoPrompt(id, p, change, evidence)`.
- Not an error: an AutoTurn that replies `[noop]`. The row is withdrawn and nothing is sent.
- `shell steward eval --variants d`: variant c plus Auto. The judge's
  `low_risk` check passes non-auto kinds with "not auto".

## 7. Plan
1. StewardRenderer [CHANGE]: `Auto` option, the auto kind line, `stewardAutoRule`, and the history outcome.
2. AutoGuard [NEW]: `autoRefusal` in `internal/rpc/suggestion.go`.
3. StewardDelivery [CHANGE]: `deliverSteward(…, autoOn, send, enqueue)`.
4. AutoTurn [NEW]: `internal/daemon/steward_auto.go` and `internal/project/steward_auto.go`.
5. ReviewEvidence [CHANGE]: auto rows counted apart.
6. LowRiskJudge [NEW]: `low_risk` check, auto kind, variant d. Run c,d on both agents.
7. Deploy with `steward_auto: "shadow"` on both agents. After a week of shadow notes, the owner switches to "on".

## 8. Done
- Tests: `go test ./...` passes. It covers the guard's three refusals, shadow vs on delivery, the AutoTurn report, [noop] and failure, and the review counts.
- Eval: `shell steward eval --variants c,d` has run on both agents. Every auto cell passes `low_risk`, and d does not drop below c on `grounded`.
- Shadow live: "daemon starting" appears in both daemon.logs, and both configs have `steward_auto: "shadow"`. Shadow notes are expected in the owner's DM on later deep beats.

## Results
Run on 2026-10-17: variants c and d, both agents, 1 sample per cell, $3.16
in total. On Pika, the $2 budget ran out before 2 projects (4 cells), which
are not counted.[^E2]

| set | cells graded | none | do/ask/goal/auto | new | agent_led | grounded | low_risk | format | all pass |
|---|---|---|---|---|---|---|---|---|---|
| c, both agents | 12 | 2 | 7/4/1/0 | 8/12 | 9/12 | 11/12 | 12/12 | 6/12 | 3/12 |
| d, both agents | 10 | 4 | 2/2/0/6 | 7/10 | 8/10 | 9/10 | 10/10 | 8/10 | 5/10 |

What the table shows:
- **Auto takes over from do.** In d, 6 of the 10 graded proposals are auto.
  Five of them are research or a new draft: Kohl's items for the expiring
  store credit, a week's training plan, a game review, a flight comparison,
  and a deck list. The sixth is a side-by-side team page.
- **Every auto proposal passes `low_risk`.** None buys, sends, books or
  edits anything the family has.
- **The other checks hold.** New (70% vs 67%) and grounded (90% vs 92%)
  match c within noise at n≈10. Format is higher, because auto needs no question.
- **What stays do.** The steward kept a test message to a person's DM, and
  a code change, as do, not auto. That is the line the rules draw.
- Caveat: as in the first eval, the generator has no tools. Whether the
  turn itself stays inside the rules is checked live in shadow mode first.

[^E2]: Eval runs · `ls ~/.shell/steward-eval/` · `20261017-084854-pikamini`, `20261017-085122-umbreonmini`; `jq 'select(.variant=="d")' cells.jsonl` shows each auto change and the judge's `low_risk` reason.

[^E1]: In-thread steward proposals · `sqlite3 ~/.shell/agents/<agent>/shell.db "select id,status,project from suggestions where source='steward' and audience like 'chat:%'"` · on 10/17, 7 thread posts (Pika #25–29, Umbreon #21–22); 6 still `delivered` with no answer, and #29 (a dev project) `accepted`.
