# Research: does block/buzz fit pika and umbreon?

## Research Question

Could [block/buzz](https://github.com/block/buzz) — a self-hostable Nostr relay
workspace where agents are cryptographic members — serve as the message
transport or agent substrate for the two family agents, replacing or
supplementing Telegram? Evaluated 2026-08-08 for a two-user household: one
engineer, one non-technical daily user.

## Summary

Not now, in any form that puts buzz inside shell's operational envelope. The
blocker is not integration difficulty — the intake seam is transport-neutral and
a new producer is small work. It is that the shipping mobile client appears to
lack push, which is the delivery mechanism the non-technical user depends on,
and that an additional transport has no user today: the engineer already reaches
both agents via Telegram and `shell chat`.

**Framing correction (owner, 2026-08-08):** buzz need not be self-hosted. Run by
someone else — as Telegram's servers are — it becomes a transport question, and
the container floor below stops being a shell concern. Those findings describe
self-hosting accurately but are not the reason to decline.

One idea is worth reading without adopting: NIP-AE agent engrams, as a design
comparison for ghost's memory injection.

## Findings

### Hosted relays exist, and the CLI ships with the desktop cask

Verified 2026-08-08 by installing. buzz.xyz offers Block-hosted relays, so the
container floor below is optional. `brew install --cask block-buzz` (Buzz 0.5.7,
signed by Block, Inc. EYF346PHUG) bundles `buzz`, `buzz-acp`, `buzz-agent` and
`buzz-dev-mcp` in `Contents/MacOS/` — no Rust toolchain is needed despite the
CLI README documenting only `cargo install`. Two gotchas: the npm package named
`buzz-cli` is an unrelated Vue tool, and the bundled binary is quarantined, so
running it from a shell hangs until it is copied out and the xattr cleared.
Hosted communities are invite-only with per-community relay URLs; no public
endpoint resolves.

### Self-hosting's floor is five containers, if you choose it

`deploy/compose/` defines relay, Postgres 17, Redis 7, MinIO and a minio-init
bootstrap. `deploy/compose/README.md` calls these real dependencies "today";
there is no SQLite or single-binary path, and `ghcr.io/block/buzz:main` is a
moving tag with no semver server release.

### Push notification support could not be confirmed in the shipping client

Research found `AppDelegate.swift` requesting `[.badge]` only and never calling
`registerForRemoteNotifications()`, no `POST_NOTIFICATIONS` and no FCM service
in the Android manifest, and no push plugin in `pubspec.yaml`. NIP-PL (draft)
specifies a content-free reconnect instruction by design, so no lock-screen
preview even once push lands. **This contradicts Block's engineering blog**,
and the check was not exhaustive across all Dart files — see Open Questions.

### Immutable signed events conflict with streaming replies

Every buzz action is a signed, immutable Nostr event. shell's `Sink.Update`
streams a reply by repeatedly revising it in place. On buzz that requires
emitting kind:40003 edit events — dozens per reply, permanently, into the
audit log — or making `Update` a no-op and losing streaming.

### The agent layer would displace shell's session machinery

`buzz-acp` owns the agent subprocess, where shell's per-(chat,thread)
`claude --resume` sessions, ExecutionProfile, model routing, rotation and ghost
injection live. Adopting it replaces them rather than integrating. Issue #2663
(a receive path for external long-lived processes) is the prerequisite for any
integration that keeps shell's agent layer.

### shell's intake seam is not the obstacle

Verified in code: `RegisterTransport`, `Sink` and `MessageTurn` are
transport-neutral, and a producer registers by implementing three methods. This
is why "buzz is hard to add" is *not* a reason given here — difficulty is low;
demand and operational cost are the problems.

### A live regression was found during this review (since fixed)

Drain phase 2 was dead on both agents: `waitDeliveries` counted `pending_turns`,
but the 2026-08-07 cutover routed writes to the queue ledger, so it read 0
forever and the barrier passed instantly — silently disabling the fix for the
2026-08-01 replay incident. Fixed by routing the read through `TurnLedger`.

## Code References

- `internal/bridge/drain.go:63` — `waitDeliveries` reads the raw store
- `internal/bridge/reactions.go:366` — `BeginPendingTurn` routes to `turnLedger`
- `internal/store/pending_turns.go:98` — `UndeliveredSince` counts `pending_turns`
- `internal/scheduler/messageturn.go` — `Sink`, `MessageTurn`, `RegisterTransport`
- `internal/daemon/telegramqueue.go` — the Telegram producer, as a model
- `internal/daemon/clichat.go` — shared runner; discards `resp.Photos`/`Videos`

## Open Questions

1. Does the mobile client actually support push? Block's blog says yes; the
   client source says no. One grep across `mobile/` settles it.
2. Has issue #2663 shipped? It changes integration cost, not the verdict.
3. Would a Go Nostr client handle buzz's NIP-42 handshake and extended NIP-01
   filters? Untested. The bundled CLI is now a cheaper way to answer this than
   writing one — it can be driven as a subprocess.
