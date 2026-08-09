# Agent boundary: data model and interface

Reference for `docs/PLAN-AGENT-BOUNDARY.md`. Shows the boundary as it is today
and as the plan reshapes it. Not a spec of ACP — a spec of *our* interface,
which borrows ACP's vocabulary without adopting its wire.

## Where the boundary sits

```
Telegram / CLI / queue
        │
   internal/bridge      ← 429 lines per turn: rotation, ghost injection,
        │                 prompt assembly (6 sources), execution profile,
        │                 write-hygiene, media notes
        │  ONE CALL: Agent.Send(ctx, AgentRequest, stream) → SendResult
        ▼
   internal/process     ← subprocess lifecycle, stream-json protocol,
        │                 session bookkeeping
        ▼
   claude CLI  (today)      codex-acp  (phase 4)
```

Everything above the line stays. The plan changes only what the line carries
and who can implement it.

## Today

```go
type AgentRequest struct {
    ChatID, MessageThreadID int64
    SessionID    string            // claude session id for --resume
    Text         string
    Images       []ImageAttachment
    PDFs         []PDFAttachment
    SystemPrompt string            // 6 concatenated sources; spawn-bound
    Model        string            // per-turn override;      spawn-bound
    Effort       string            // "high" on deep beats;   spawn-bound
    Ephemeral    bool              // one-shot, never touches the session
    Timeout      time.Duration
}

type SendResult struct {
    Text         string
    SessionID    string
    Artifacts    []Artifact
    ToolCalls    []ToolCall   // collected AFTER the fact
    Usage        *Usage
    Timings      Timings      // QueueMs, FirstEventMs, TTFTMs, TotalMs
    FirstEventAt time.Time
}

type StreamFunc func(delta string)   // text only
```

Three gaps, each a phase in the plan:

1. **No stop reason.** Why a turn ended is inferred from `Usage` and error
   strings. Rotation keys on max-tokens conditions it reconstructs.
2. **Untyped stream.** Tool calls cannot be observed live; they appear in
   `SendResult` once the turn is over.
3. **No capability declaration.** Callers type-assert `Injector`
   (`inject.go:43`) to discover whether mid-turn injection exists.

## Proposed

### Stop reason (phase 2)

```go
type StopReason string

const (
    StopEndTurn   StopReason = "end_turn"   // model finished normally
    StopMaxTokens StopReason = "max_tokens" // budget hit — rotation's signal
    StopCancelled StopReason = "cancelled"  // caller cancelled
    StopRefusal   StopReason = "refusal"    // model declined
    StopError     StopReason = "error"      // transport or runtime failure
)
```

Added to `SendResult`. `Manager` derives it from what the CLI already reports,
so no behaviour changes — the difference is that rotation reads a reason
instead of rebuilding one.

### Typed stream events (phase 2)

```go
type StreamEvent interface{ isStreamEvent() }

type TextDelta   struct{ Text string }
type ToolStarted struct{ ID, Name string; Input json.RawMessage }
type ToolDone    struct{ ID string; Err string }   // Err == "" means success
type UsageUpdate struct{ Usage Usage }

type StreamFunc2 func(StreamEvent)
```

The existing `StreamFunc` becomes an adapter that forwards only `TextDelta`, so
no caller changes in phase 2. Modelled on ACP's `session/update` variants
(`agent_message_chunk`, `tool_call`, `tool_call_update`, `usage_update`).

### Capabilities (phase 3)

```go
type Capabilities struct {
    Images    bool
    PDFs      bool
    Streaming bool
    Injection bool  // replaces the Injector type-assertion
    Models    []string // empty = runtime default only
}

// Added to Agent:
Capabilities() Capabilities
```

Lets the bridge degrade deliberately — a runtime without image support gets a
described photo rather than a dropped one — and lets a fake declare a narrow
surface so tests exercise the fallback paths.

### Fake (phase 3)

```go
type Fake struct{ Script []ScriptedTurn }

type ScriptedTurn struct {
    Expect string        // substring the prompt must contain ("" = any)
    Events []StreamEvent
    Result SendResult
}
```

Implements `Agent` with no subprocess and no network, so a bridge turn is
testable end to end.

### Native loop (phase 5)

Where the abstraction pays off: a third implementation in which shell owns the
loop and the model becomes a provider setting.

```go
type NativeAgent struct {
    Provider ModelProvider   // Anthropic, OpenAI, local — swappable here
    Tools    ToolRegistry    // MCP servers (go-sdk v1.7.0) + builtins
}

type ModelProvider interface {
    Complete(ctx context.Context, req Completion, emit func(StreamEvent)) (StopReason, error)
    Models() []string
}
```

The loop is prompt → model → tool calls → execute → repeat. Note what shell
would be taking over from Claude Code: Bash, Read, Write, Edit and web access.
MCP covers most of it — shell is already an MCP client — but the builtins and
their failure handling become ours.

This is where "swap the model" becomes real: today model choice is a CLI flag
that only Claude Code honours; here it is a provider call.

### ACP implementation (phase 4)

| our concept | ACP carrier | notes |
|---|---|---|
| `Send` | `session/prompt` | prompt as content blocks |
| stream events | `session/update` | direct variant mapping |
| `StopReason` | `StopReason` | ACP already has this |
| `Model` | `SetSessionConfigOption` | **standard**; values agent-specific |
| `SystemPrompt` | `_meta` | **extension**; honoured by convention |
| `Effort` | unverified | check before relying on it |
| `SessionID` | ACP session id | `session/load` for resume |
| rotation | `session/close` + `session/new` | mirrors `KillProcess` |
| `Injector` | steering (`_meta`) | claude-agent-acp advertises it |

Two rows carry risk: `SystemPrompt` is extension-only, and `Effort` has not
been confirmed to exist as a config option on any runtime.

## Invariants

1. The Claude CLI path stays default and behaviourally unchanged.
2. Nothing above the boundary moves — the 429 lines are out of scope.
3. Every new field is additive; phase 2 changes no call site.
4. A runtime that cannot do something declares it rather than failing at use.
