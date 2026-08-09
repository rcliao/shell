# Research: could shell's bridge↔process boundary become ACP?

## Research Question

ACP standardises client↔agent communication and is implemented by Claude,
codex and goose. Buzz uses it where shell uses a bespoke boundary.

Q1. What crosses shell's bridge↔process boundary today, and which tuned
subsystems depend on its shape?

Q2. Can core ACP carry that, or would the parts shell relies on live outside
the standard?

## Summary

ACP is a real multi-vendor standard, but its core session model cannot express
what shell's boundary carries. `NewSessionRequest` accepts only `_meta`,
`additionalDirectories`, `cwd` and `mcpServers` — there is no system prompt, no
model and no effort in the standard. Buzz passes the system prompt through
`_meta`, a vendor extension, so even its working integration is not portable
between ACP agents.

Since shell's per-turn model routing, effort and rebuilt system prompt are all
spawn-bound, adopting ACP would move them into the same extension namespace,
buying the protocol's shape without its interoperability.

## Findings

### [Q1] The boundary is one call carrying eleven fields

`Agent.Send(ctx, AgentRequest, StreamFunc) (SendResult, error)` is the whole
turn path (`internal/process/agent.go:46`). `AgentRequest`
(`agent.go:23`) carries chat/thread, session id, text, images, PDFs,
`SystemPrompt`, `Model`, `Effort`, `Ephemeral` and `Timeout`. `SendResult`
(`manager.go:46`) returns text, session id, artifacts, tool calls, usage and
four-phase `Timings`. The interface already declares itself swappable —
"so the implementation can be swapped (e.g. Claude CLI, HTTP API, mock)".

### [Q1] Three of those fields bind only at spawn

`internal/process/args.go:39` states it plainly: `--model`, `--effort` and
`--append-system-prompt` "take effect only at spawn. A resumed session
(`--resume`) already carries its system prompt." This is why rotation exists as
`KillProcess` (`agent.go:65`) — forcing a rebuilt prompt to load means killing
the subprocess, not reconfiguring it.

### [Q1] The tuned subsystems sit on the bridge side, not the process side

ExecutionProfile (`internal/bridge/execution.go:21`) resolves model, effort,
ephemerality, timeout and task type per turn, then hands them across as request
fields. Rotation, prewarm, prompt fingerprinting and ghost injection all live in
`internal/bridge/`. The process layer owns subprocess lifecycle, the
stream-json protocol and session bookkeeping — a genuinely thin waist.

### [Q2] Core ACP has no system prompt, model or effort

From the v1 schema: `NewSessionRequest` properties are exactly `_meta`,
`additionalDirectories`, `cwd`, `mcpServers`. `LoadSessionRequest` adds only
`sessionId`. Configuration is `SetSessionConfigOptionRequest{_meta, configId,
sessionId}` with boolean and select option types, and `SetSessionModeRequest`
for modes. None of these carries a model id, an effort level, or prompt text.

### [Q2] Buzz's own integration proves the gap

The live handshake advertised `promptCapabilities:{embeddedContext, image}` and
session `resume/fork/list/close/delete`. But buzz-acp sends the system prompt as
`_meta.systemPrompt.append` on `session/new`, and applies `--model` "to every
new ACP session after creation". Both sit outside core ACP, so buzz's agents are
portable only across agents that honour the same extensions.

### [Q2] What shell would gain is a test seam, not interop

The credible win is a typed, documented protocol boundary with an off-the-shelf
fake, since `Agent` is today only faked by hand. That is achievable by
tightening the existing interface — which already anticipates substitution —
without adopting a protocol whose standard part omits what shell sends.

## Code References

- `internal/process/agent.go:23,44` — `AgentRequest`, the `Agent` interface
- `internal/process/args.go:39` — spawn-binding of model/effort/system prompt
- `internal/process/manager.go:46` — `SendResult`, `Timings`
- `internal/bridge/execution.go:21` — `ExecutionProfile` per-turn resolution
- ACP v1 `schema/v1/schema.json` — `NewSessionRequest`, `SetSessionConfigOption`

## Open Questions

1. Does ACP intend model selection to arrive as a `SessionConfigSelect` option?
   If so, the gap may close and this should be re-checked.
2. Could `_meta` extensions be acceptable if shell only ever drives
   claude-agent-acp — i.e. is portability actually wanted, or only the shape?
3. What does shell lose by inserting an adapter process between bridge and
   Claude — latency, prewarm control, rotation timing? Unmeasured.
