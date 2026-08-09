# Research: could shell's bridge↔process boundary become ACP?

Second pass, 2026-08-08. The first pass concluded that core ACP "cannot carry"
what shell sends; that framing was wrong and is corrected below.

## Research Question

Q1. Is ACP generic enough to express what shell needs, or does it standardise a
different division of responsibility?

Q2. What does shell's bridge actually do around the boundary, and what would a
swap touch?

## Summary

ACP is generic where it standardises — prompts, streaming, tools, permissions,
filesystem, terminals — and deliberately silent about model, effort and system
prompt. That silence is a design position: **the agent owns how it thinks, the
client owns the conversation.** Shell inverts that, resolving model, effort and
a six-source system prompt per turn on the bridge side.

So the mismatch is architectural, not a missing field. ACP's `_meta` and `Ext*`
are sanctioned extension points, so shell *could* comply — but the semantics of
those keys are unstandardised, so any portability gained would be nominal.

## Findings

### [Q1] `_meta` is sanctioned extensibility, not a vendor hack

Correcting the first pass. The schema states `_meta` is "reserved by ACP to
allow clients and agents to attach additional metadata", and `ExtRequest` /
`ExtNotification` exist for arbitrary methods "while maintaining protocol
compatibility". Buzz passing `_meta.systemPrompt.append` is the designed path.

The catch is the next sentence: implementations "MUST NOT make assumptions
about values at these keys". An agent that does not recognise the key ignores
it. Compliance is preserved; meaning is not.

### [Q1] The omission is stable, and the lifecycle is LSP's, not MCP's

MCP `2026-07-28` deleted its handshake — "there is no negotiation handshake" —
and now calls `initialize`-based revisions legacy. ACP has not followed and
shows no sign of it: v2 retains `InitializeRequest`, and searching their issues
for "stateless" or "handshake" returns nothing. Their MCP alignment work
(#1576, #1368) tracks *content types* and transports instead.

That is coherent rather than stale. MCP dropped the handshake to allow
stateless HTTP servers behind load balancers; ACP is stdio with one spawned
subprocess per connection, where the process is the session and a handshake
costs one round trip on a pipe you own. Its `initialize` is LSP heritage.

### [Q1] ACP standardises the conversation, not the configuration

24 requests and 7 notifications cover sessions, prompts, cancellation,
permissions, terminals, filesystem, elicitation and auth. `NewSessionRequest`
carries `cwd`, `mcpServers`, `additionalDirectories`; `PromptRequest` carries
`sessionId` and `prompt`. Configuration is `SetSessionConfigOption` and
`SetSessionMode`. Nothing names a model or system prompt — consistently.

### [Q2] The boundary is thin; the bridge around it is thick

`HandleMessageStreaming` is **429 lines around a single `agent.Send`** at
relative line 299. Before it: plan handling, `ensureSession`, `maybeRotate`,
ghost injection, `buildPerTurnBlocks`, heartbeat enrichment, system-session
pre-emption, onboarding, **six `systemPrompt +=` sources**, session tracking and
`resolveExecutionProfile`. After it: media-note extraction and
`processResponse`.

A swap replaces what `Send` talks to and leaves all 429 lines standing.

### [Q2] Three fields bind at spawn, and rotation is built on that

`args.go:39`: `--model`, `--effort` and `--append-system-prompt` "take effect
only at spawn. A resumed session already carries its system prompt." Rotation is
therefore `KillProcess` (`agent.go:65`) — kill to reload a prompt. Under ACP the
equivalent is closing and recreating a session, which is available
(`sessionCapabilities.close`), so the mechanism ports; the per-turn *values*
would have to travel in `_meta`.

### [Q2] There is exactly one implementation and no fake

`var _ Agent = (*Manager)(nil)` is the only binding, and no test implements the
interface — bridge tests exercise other paths instead. `Injector` was
deliberately kept *out* of `Agent` "so test doubles and alternative agents don't
have to implement it", which shows the seam was designed for substitution that
never happened.

## Code References

- `internal/process/agent.go:23,44,65,78` — request, interface, `KillProcess`
- `internal/process/args.go:39` — spawn-binding of model/effort/prompt
- `internal/process/inject.go:43` — `Injector` kept outside the interface
- `internal/bridge/bridge.go:747` — the 429-line turn, one `Send` at ~299
- `internal/bridge/execution.go:21` — per-turn `ExecutionProfile`
- ACP v1 `schema/v1/schema.json` — 168 defs; `NewSessionRequest`, `PromptRequest`

## Open Questions

1. ~~Model selection via `SessionConfigSelect`?~~ **Answered:** v2 (172 defs)
   still has no model, effort or system prompt. Stable, not immaturity.
2. Do we want portability across agent runtimes, or only a cleaner shape? Only
   the first justifies ACP; the second is reachable by tightening `Agent`.
3. What does an adapter process cost in latency, prewarm control and rotation
   timing? Entirely unmeasured, and it sits on the family's turn path.
