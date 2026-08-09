# Plan: an ACP-shaped agent boundary

## Overview

Reshape `process.Agent` so a second runtime — Codex via `codex-acp`, keeping
pikamini's identity, memory and prompts — becomes a configuration choice rather
than a rewrite. Borrow ACP's vocabulary (stop reasons, typed stream events,
declared capabilities) inside our own interface first, then add an ACP-speaking
implementation behind it. When done, `Agent` has two implementations and a test
fake, and the family's turns still run on the Claude CLI path unchanged.

## Current State

`Agent` (`internal/process/agent.go:44`) has one implementation,
`var _ Agent = (*Manager)(nil)` (`:78`), and no test fake. `Send` returns
`(SendResult, error)` with no stop reason; the stream is
`func(delta string)` — text only, so tool calls surface post hoc in
`SendResult.ToolCalls` (`manager.go:46`). `Injector` sits outside the interface
(`inject.go:43`) "so test doubles and alternative agents don't have to
implement it". `HandleMessageStreaming` (`bridge.go:747`) is 429 lines around a
single `Send`; model and effort resolve per turn via `ExecutionProfile`
(`execution.go:21`), and all three spawn-bind (`args.go:39`).

## Desired End State

Two runtimes answer the same interface, selected by config, with the Claude path
byte-identical in behaviour. Verified by: a stored transcript replays through
the fake with no subprocess; `shell chat` returns the same reply shape on either
runtime; and a canary agent runs a full day on Codex with no regression in the
e2e timings already recorded in `message_map`.

## What We're NOT Doing

Not adopting ACP as shell's own wire protocol — the bridge keeps calling a Go
interface, not JSON-RPC. Not moving the 429 lines of turn preparation; ghost
injection, rotation, prompt assembly and write-hygiene stay exactly where they
are. Not replacing the Claude CLI path, which stays default. Not putting an
adapter process in front of Claude — ACP is for the *second* runtime only. Not
touching Telegram, the queue, or scheduling.

## Implementation Phases

### Phase 1 — Measure the baseline before changing anything

Capture current per-turn latency distribution from `message_map`
(`e2e_first_visible_ms`, `e2e_total_ms`) as the comparison set. Any later phase
that regresses these is reverted, not tuned.

**Automated:** a command prints p50/p95 over a stated window.
**Manual:** the numbers look like the turns we remember.

### Phase 2 — Give the boundary a vocabulary

Add to `SendResult` a `StopReason` (`end_turn | max_tokens | cancelled |
refusal | error`) which `Manager` derives from what the CLI already reports, and
replace the bare `StreamFunc` with typed events (text delta, tool call, tool
result, usage). Keep the old callback as an adapter so no caller changes yet.

**Automated:** existing tests pass untouched; new tests assert each stop reason
maps from a real CLI transcript.
**Manual:** rotation's max-tokens path now reads a reason instead of inferring.

### Phase 3 — Declare capabilities and ship a fake

Add `Capabilities()` (images, PDFs, streaming, injection) so callers stop
type-asserting `Injector`. Ship `process.Fake` implementing `Agent` from a
scripted transcript.

**Automated:** a bridge test drives a full turn through `Fake` with no
subprocess and no network.
**Manual:** the fake is convincing enough to write a new bridge test against.

### Phase 4 — Add the ACP implementation

`process.ACPAgent` speaking ACP over stdio to `codex-acp`: `initialize`,
`session/new`, `session/prompt`, `session/update` → typed events, model via
`SetSessionConfigOption`, system prompt via `_meta`, rotation via session close.

**Automated:** contract tests run against both implementations.
**Manual:** `shell chat --config <canary>` holds a coherent multi-turn
conversation on Codex.

### Phase 5 — Canary

Run one non-family agent on the ACP path for a day. Compare against Phase 1.

**Automated:** latency comparison within the recorded band.
**Manual:** owner reads a day of transcripts and finds nothing degraded.

## Risks

**Phase 4 is speculative until Phase 3 exists.** If the fake proves the
interface is wrong, stop after Phase 3 — it delivers the testability win alone.

**The system prompt is `_meta`-only**, so on Codex it is honoured by convention.
Accepted: verified in Phase 4's manual check, not assumed.

**Scope creep into the 429 lines.** The fence above is the mitigation; a change
there is a separate plan.
