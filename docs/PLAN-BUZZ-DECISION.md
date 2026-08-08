# Plan: decline buzz inside shell, re-arm the drain barrier

## Overview

Records the decision not to adopt block/buzz in any form, and executes the one
piece of work this evaluation actually justified: restoring drain phase 2, which
the 2026-08-07 queue cutover silently disabled on both live agents. When this
plan is done, a deploy again waits for in-flight replies to reach Telegram
before re-execing, the decision against buzz is written down with the signal
that would reopen it, and one bounded design read of NIP-AE has happened for
ghost's benefit. No new services, no new dependencies, no buzz code.

## Current State

Drain has two phases. Phase 1 waits for turns to finish
(`internal/bridge/drain.go:39`). Phase 2 waits for replies to be *delivered*,
via `waitDeliveries` at `internal/bridge/drain.go:63`, which calls
`UndeliveredSince` — a count of undone `pending_turns` rows
(`internal/store/pending_turns.go:98`).

Since the cutover, `BeginPendingTurn` routes to the queue ledger whenever
`turnLedger` is set (`internal/bridge/reactions.go:366`), and both live configs
enable `telegram_queue_intake`. Nothing writes `pending_turns` anymore, so the
count is permanently 0 and phase 2 returns immediately. Verified: `pending_turns`
frozen at 2026-08-08 01:17 (pika) and 2026-08-07 16:12 (umbreon), while
telegram-sourced tasks continue past both.

## Desired End State

Drain phase 2 blocks on genuinely undelivered turns again, regardless of which
ledger is active. Verified by a test that fails against the current code, and by
a deploy log showing a non-zero wait when a turn is in flight — not merely by
the absence of complaints, which is exactly how this went unnoticed.

## What We're NOT Doing

Not adopting buzz into shell: not as a transport, not as an agent substrate,
not self-hosting its stack. The fence covers shell's operational envelope only —
buzz as a standalone experiment, or against someone else's relay, is a separate
question this plan neither answers nor forbids. Not adding cryptographic agent
identity or a signed event log; both daemons hold the key, so signing buys
nothing. Not adopting buzz-workflow — the queue already covers it, and that
framing risks rebuilding the `agent.task` kind retired on 2026-08-07. Not
re-litigating the ledger cutover, which is working.

## Implementation Phases

### Phase 1 — Make drain phase 2 ledger-aware

Give `TurnLedger` an undelivered-count method and have `waitDeliveries` ask the
active ledger instead of the raw store, so the queue-backed ledger answers from
`tasks` (leased, telegram-sourced, within the grace window) and the legacy one
keeps its `pending_turns` query. The seam already exists at
`internal/bridge/reactions.go:366`; this extends it rather than adding a branch
at the call site.

**Success criteria (automated):** a test that counts an in-flight queue turn as
undelivered and fails against current code; `go test ./...` green; `go vet`
clean. Confirm the test fails when the ledger-aware path is reverted.

**Success criteria (manual):** on a canary agent, restart while a turn is
mid-generation and see a non-zero delivery wait in the drain log, then the reply
arriving intact.

### Phase 2 — Record the decision

Land `docs/RESEARCH-BUZZ-FIT.md` and this plan, and note in `docs/TASKS.md` that
external transports were evaluated and declined, with the revisit trigger.

**Success criteria (automated):** `comments validate` passes for both docs
against their templates; `make verify-no-pii` passes unpiped.

**Success criteria (manual):** a reader who has not seen this conversation can
tell why buzz was declined and what would change it.

### Phase 3 — Read NIP-AE for ghost (timeboxed, 2 hours)

Read buzz's NIP-AE agent-engram spec (kind:30174: one `core` identity record
plus scoped `memory` records) purely as a design comparison for ghost's pinned
injection, which currently flags a rotation instead of injecting when it
overflows its token budget. Adopt no code and add no dependency.

**Success criteria (automated):** none — this is a read, and claiming otherwise
would invent a metric.

**Success criteria (manual):** either a written note on whether a core/memory
split helps ghost's budget overflow, or an explicit "no, and here is why."

## Risks

**The drain fix could over-wait and stall deploys.** A queue turn stuck `leased`
would block every restart until timeout. Mitigated by reusing the existing
5-minute grace window, so only recent turns count.

**Accepted:** the mobile-push finding rests on second-hand research
contradicted by Block's blog, and the ops-floor argument only applies to
self-hosting. What survives both caveats is that no second user needs this
channel today, so the decision rests there and the rest is recorded as open.
