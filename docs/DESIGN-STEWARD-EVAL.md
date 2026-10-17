# Steward proposal eval

Status: shipped (2026-10-16); variant c is the production default. Follows DESIGN-AGENT-LOOPS.md step 1 (the
project steward). Out of scope: where proposals are posted, how many per
beat, and the delivery code.

## 1. Intent
Proposals agents can act on.

## 2. Decisions
- **Offline eval over live A/B.** Live answers take days and the family is
  small. Offline runs compare prompts on the same inputs in minutes.[^E1]
- **One renderer for production and eval over an eval-only copy.** The
  variants are switches on `StewardOptions`, so shipping the winner means
  changing the default.
- **Grade each proposal with a pass/fail rubric over one 1–10 score.** Each
  check names one failure, so a failing check points at the prompt change.
- **Recommend a default, still ask yes/no, over "unless you object".**
  Nothing proceeds on silence today. An unanswered proposal just expires
  after 14 days, so the prompt must not promise that it will.
- **Eval ignores the 14-day open-proposal skip.** It runs as a beat would
  after the skip expires. Baseline sees nothing of the expired proposal
  (production behavior). Variants A and B see it in the history lines. That
  difference is what is being tested.

## 3. Components
- **StewardRenderer**: renders project entries and steward instructions.
- **AskHistory**: lists a project's earlier steward proposals, any status.
- **EvalSnapshot**: a read-only copy of the agent's shell.db.
- **Generator**: runs one variant on one project, returns one proposal.
- **FormatCheck**: deterministic checks on a proposal's family text.
- **ProposalJudge**: separate model; grades the rubric from the doc and thread.
- **EvalReport**: the per-variant pass-rate table plus per-cell JSONL.

## 4. Data flow
1. The owner runs `shell steward eval`; EvalSnapshot copies shell.db into the results directory.
2. For each active project the agent owns, StewardRenderer renders a one-project steward block per variant.
3. Generator adds the project doc (what doc-read returns) and an offline footer, calls `claude -p` with the agent's own model and no tools, and parses one proposal or "none".
4. FormatCheck checks the proposal's `change`.
5. ProposalJudge gets the doc, the thread's messages from the last 30 days, the AskHistory before the cutoff, and the proposal, and returns the rubric checks.
6. EvalReport writes `cells.jsonl` and prints the table. In `--baseline` mode, steps 2–3 are skipped: the agent's real steward proposals are graded, each with a cutoff at its own `created_at` and the doc as of that time.

## 5. Data model
| thing | why |
|---|---|
| `~/.shell/steward-eval/<ts>-<agent>/cells.jsonl` | one row per (project, variant, sample): prompt, raw output, proposal, checks, cost |
| no schema change | AskHistory reads the existing `suggestions` rows |

## 6. Interfaces
- `bridge.StewardEntry(st, p, doc, now, opts) → string` and
  `bridge.StewardInstructions(opts, max) → string`. Production calls them with
  `DefaultStewardOptions`. An empty entry never happens; a store error drops
  that line.
- `store.StewardForProject(slug, since, before) → []Suggestion` (oldest first).
  An empty result means nothing was asked before.
- `shell steward eval --config <agent config> [--variants base,a,b]
  [--samples 1] [--budget 5] [--baseline] [--project slug]`. The caller handles
  these failures:
  - A `claude -p` failure: the cell is recorded with `error` and is not
    counted.
  - A spent budget: the run stops and reports a partial table.
  - "none": counted as abstention, not a failure.

Rubric (ProposalJudge; each check is pass or fail with a one-line reason):
- **new**: not a decision already put to the family in the thread or in an
  earlier proposal, unless the proposal names something new since then.
- **agent_led**: after a one-word yes the agent can take the next step
  alone. An ask passes only when the choice really is the family's (money,
  taste, dates they commit to, consent).
- **useful**: moves the 目標 or a deadline, and says what the family gets.
- **concrete**: names the first action. A goal also names a cadence and a
  done check.
- **grounded**: every fact in `change` is in the doc, the thread or the
  evidence.

FormatCheck (in code, no model):
- at most 2 sentences
- contains a question
- no links, emails, phone numbers or `#ids`
- at most 200 characters
- written in the project's language (skipped when lang is empty)

Variants:
- **base**: the current prompt.
- **a**: base plus each project's AskHistory lines, the rule "don't re-ask
  without something new", and, for a question already unanswered twice,
  "do the work that makes it easy, or recommend one answer and ask for a
  好".
- **b**: a plus what the agent can do alone, a preference for do/goal (ask
  only for money, taste, commitments or consent) and a self-check before
  filing.
- **c**: b plus "keep change under 200 characters". It ships as the
  default.

## 7. Plan
1. AskHistory [NEW]: one query on `suggestions`.
2. StewardRenderer [CHANGE]: split `buildStewardBlock` into entry and
   instructions, with `StewardOptions`; production output is unchanged for
   the default options.
3. EvalSnapshot, Generator, FormatCheck, ProposalJudge, EvalReport [NEW]:
   `cmd/shell/steward_eval.go` and `internal/bridge/steward_eval.go`.
4. Run the baseline on both agents. Check that the judge fails the known
   repeats. Then run base/a/b.
5. Make the winner (c) the default, merge, deploy, and watch the answer
   rate in the weekly review.

## 8. Done
- Baseline graded: proof: `shell steward eval --baseline` on both agents →
  table. Invariant: the known repeats fail `new`.
- Variant table: proof: one run of base,a,b on both agents → table. The
  winner beats base on `new` and `agent_led` and does not drop on `grounded`
  or format.
- Shipped: proof: `go test ./internal/bridge/ ./internal/store/ ./cmd/shell/`
  passes; "daemon starting" appears in both daemon.logs after deploy;
  `DefaultStewardOptions` equals the winner.

## Results
Run on 2026-10-16. 1 sample per cell, about $9.50 in total. Each cell
counts one project, one variant and one generation. The judge grades only
the cells that filed a proposal. Pika has 13 owned projects; Umbreon has 3.
The cells, with the full prompts, are in `~/.shell/steward-eval/`.[^E2]

| set | cells | none | do/ask/goal | new | agent_led | useful | concrete | grounded | format | all pass |
|---|---|---|---|---|---|---|---|---|---|---|
| filed proposals, all | 15 | 0 | 1/14/0 | 8/15 | 12/15 | 13/15 | 11/15 | 12/15 | 4/15 | 1/15 |
| base, both agents | 16 | 1 | 4/10/1 | 8/15 | 10/15 | 12/15 | 9/15 | 13/15 | 4/15 | 1/15 |
| a, both agents | 16 | 4 | 5/6/1 | 6/12 | 10/12 | 10/12 | 8/12 | 11/12 | 5/12 | 1/12 |
| b, both agents | 16 | 4 | 8/3/1 | 9/12 | 12/12 | 10/12 | 10/12 | 12/12 | 8/12 | 5/12 |
| b + thread questions, both | 16 | 4 | 8/2/2 | 7/12 | 11/12 | 10/12 | 9/12 | 10/12 | 8/12 | 1/12 |
| base, Pika only | 13 | 1 | 3/8/1 | 7/12 | 9/12 | 10/12 | 7/12 | 11/12 | 3/12 | 1/12 |
| b, Pika only | 13 | 3 | 6/3/1 | 7/10 | 10/10 | 8/10 | 8/10 | 10/10 | 6/10 | 3/10 |
| c, Pika only (shipped) | 13 | 2 | 8/3/0 | 7/11 | 9/11 | 8/11 | 10/11 | 11/11 | 8/11 | 3/11 |

What the table shows:
- **Judge calibration.** On the filed proposals, the judge failed `new` on
  the known repeats: the slot-6 question, the Japan dates question and the
  Frosthaven question. It passed the first ask, and the follow-up that
  brought finished work.
- **Base reproduces the problem.** Two thirds of its proposals are ask, and
  only 4 of 15 pass the format checks.
- **a alone does not help.** With the history lines, the model abstains
  more often, but it does not get more agent-led.
- **b is the real step.** Ask falls to 3 of 12, agent_led reaches 12 of 12,
  and all-pass goes from 1 to 5. Grounding does not drop.
- **Thread questions add nothing.** The variant that also listed the
  agent's own questions from the thread re-asked them anyway, and scored
  lower on `new`. It was dropped.
- **c equals b within noise and fixes length.** It removes b's
  over-length failures.
- **Caveats.**
  - n is about 12 per variant, so differences under about 25 points are
    noise.
  - The generator has no tools here, unlike a real beat, and sees the doc
    directly.
  - The 4-cell gap between base and the other variants comes from
    abstentions ("none"). The history lines lead the model to skip projects
    it already asked about, which is what the variants are meant to do.

What is left: in b and c, the `new` failures are questions the agent had
already asked in the thread outside any proposal. The live answer rate in
the weekly review is the next measure.

[^E1]: Live steward proposals since 10/15 · `sqlite3 ~/.shell/agents/<agent>/shell.db "select id,status from suggestions where source='steward'"` · the 4 thread posts are all `delivered`, with no answer after 1–2 days.
[^E2]: Eval runs · `ls ~/.shell/steward-eval/` · `20261016-182001-umbreonmini` and `20261016-182048-pikamini` (baseline), `20261016-182157-umbreonmini` and `20261016-182313-pikamini` (base/a/b), `20261016-182810-umbreonmini` and `20261016-182845-pikamini` (b + thread questions, dropped), `20261016-183055-pikamini` (c).
