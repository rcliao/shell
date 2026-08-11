# Plan: an ACP-shaped agent boundary

Status: **accepted, scoped to phases 1-3** (owner, 2026-08-10). Phases 4-5 are
deferred, not rejected — see Decision below.

## Overview

Reshape `process.Agent` into an abstraction that can carry more than one
runtime. Borrow ACP's vocabulary — stop reasons, typed stream events, declared
capabilities — inside our own Go interface, not as a wire protocol.

Behind it sits the Claude CLI today, and later an ACP runtime or a native loop
if either is ever wanted. Each is one implementation of one interface. The 429
lines of turn preparation never move.

## Decision

Do phases 1-3: measure the baseline, give the boundary a vocabulary, declare
capabilities and ship a fake. These improve an abstraction we own, need no new
dependency, and are worth doing whether or not a second runtime ever arrives.
Finish and clean up before considering more.

Phases 4-5 stay written down because the research behind them is expensive and
perishable.

Revisit phase 4 when a second runtime is actually wanted. The first step then is
`acp-probe` against `codex-acp`, to settle whether it honours
`_meta.systemPrompt`. That is unverified today and would decide the phase.

Phase 5 needs a timeboxed spike before it can be costed.

## Current State

`Agent` (`internal/process/agent.go:44`) has one implementation,
`var _ Agent = (*Manager)(nil)` (`:78`), and no test fake.

`Send` returns `(SendResult, error)` with no stop reason. The stream is
`func(delta string)` — text only — so tool calls surface post hoc in
`SendResult.ToolCalls` (`manager.go:46`). `Injector` sits outside the interface
(`inject.go:43`) "so test doubles and alternative agents don't have to
implement it".

`HandleMessageStreaming` (`bridge.go:747`) is 429 lines around a single `Send`.
Model and effort resolve per turn via `ExecutionProfile` (`execution.go:21`),
and all three spawn-bind (`args.go:39`).

## Desired End State

An interface a second runtime could implement, with the Claude path unchanged
in behaviour.

Verified three ways. A bridge turn runs end to end through the fake, with no
subprocess and no network. Rotation reads a stop reason instead of inferring
one. And the e2e timings in `message_map` stay inside the band recorded in
phase 1.

## What We're NOT Doing

Not adopting ACP as shell's own wire protocol — the bridge keeps calling a Go
interface, not JSON-RPC. Not putting an adapter process in front of Claude.

Not moving the 429 lines of turn preparation: ghost injection, rotation, prompt
assembly and write-hygiene stay exactly where they are. Not replacing the
Claude CLI path, which stays default. Not touching Telegram, the queue, or
scheduling.

## Implementation Phases

### Phase 1 — Measure the baseline before changing anything

Capture current per-turn latency distribution from `message_map`
(`e2e_first_visible_ms`, `e2e_total_ms`) as the comparison set. Any later phase
that regresses these is reverted, not tuned.

**Automated:** a command prints p50/p95 over a stated window.
**Manual:** the numbers look like the turns we remember.

### Phase 2 — Give the boundary a vocabulary

Add a `StopReason` to `SendResult` — `end_turn | max_tokens | cancelled |
refusal | error` — which `Manager` derives from what the CLI already reports.

Replace the bare `StreamFunc` with typed events: text delta, tool call, tool
result, usage. The old callback stays as an adapter, so no caller changes yet.

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

`process.ACPAgent` over stdio to `codex-acp`: `initialize`, `session/new`,
`session/prompt`, `session/update` → typed events, model via
`SetSessionConfigOption`, system prompt via `_meta`, rotation via session close.
Proves the interface carries a runtime it was not designed around — the
cheapest possible test of that, since the alternative is writing a loop first.

**Automated:** contract tests pass against both implementations.
**Manual:** `shell chat --config <canary>` holds a multi-turn conversation on
Codex.

### Phase 5 — Native loop, behind the same interface

`process.NativeAgent`: shell owns prompt → model → tool calls → execute →
repeat, calling a provider SDK directly. Tools come from MCP servers, which
shell already speaks (`go-sdk v1.7.0`), plus the few builtins Claude Code
supplies today. Model choice becomes a provider setting rather than a CLI flag.

Gated on Phase 4: if the interface needed reshaping for ACP it will again here,
and an adapter teaches that cheaper than a loop does. Ends with a canary — one
non-family agent on the new path for a day.

**Automated:** same contract tests pass; a tool-calling turn runs end to end
against `Fake` with no network; canary latency within Phase 1's band.
**Manual:** a scripted multi-turn task completes with tool use, judged against
the same task on the CLI path, and a day of canary transcripts reads clean.

## Risks

**Phase 5 is larger than 1-4 combined** and most likely to be wrong. Owning the
loop means owning tool execution — Bash, Read, Write, Edit, web — which Claude
Code supplies today. Not a safety regression on paper, since these agents
already run `bypassPermissions`, but the responsibility moves to us. Mitigated
by 1-4 being shippable alone; 5 starts only if 4 shows the interface holds.

**The system prompt is `_meta`-only**, so on Codex it is honoured by convention.
Accepted: verified in Phase 4's manual check, not assumed.

**Scope creep into the 429 lines.** The fence above is the mitigation.
