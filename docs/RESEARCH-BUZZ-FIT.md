# Research: does block/buzz fit pika and umbreon?

## Research Question

Could [block/buzz](https://github.com/block/buzz) — a self-hostable Nostr relay
workspace where agents are cryptographic members — serve as the message
transport or agent substrate for the two family agents, replacing or
supplementing Telegram? Evaluated 2026-08-08 for a two-user household: one
engineer, one non-technical daily user.

## Summary

No, in all three forms considered (replace Telegram, add as a third transport,
or adopt its concepts). The blocker is not integration difficulty — shell's
intake seam is genuinely transport-neutral and a new producer is small work.
The blockers are that buzz's floor is five always-on containers against today's
one Go binary plus SQLite; that its shipping mobile client appears to lack push
notifications, which is the entire delivery mechanism the non-technical user
depends on; and that an additional transport has no user, since the engineer
already reaches both agents via Telegram and `shell chat`.

One idea is worth reading without adopting: NIP-AE agent engrams, as a design
comparison for ghost's memory injection.

## Findings

### buzz's deployment floor is five containers, with no minimal mode

`deploy/compose/` defines relay, Postgres 17, Redis 7, MinIO and a minio-init
bootstrap, plus four named volumes. `deploy/compose/README.md` states the
dependencies are real "today" and that "minimal mode can simplify this later."
There is no SQLite path and no single-binary mode. The published image
`ghcr.io/block/buzz:main` is a moving tag with no semver server release. No
measured resource footprint exists; the only published number is a Helm default
(512Mi request / 2Gi limit) sized for a team, not benchmarked at two users.

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

`buzz-acp` owns the agent subprocess. shell's per-(chat,thread) `claude --resume`
sessions, ExecutionProfile, model/effort routing, rotation and ghost injection
live in that layer. Adopting buzz-acp replaces them rather than integrating.
Issue #2663 (a supported receive path for external long-lived processes) is the
prerequisite for any integration that keeps shell's agent layer.

### shell's intake seam is not the obstacle

Verified in code: `RegisterTransport`, `Sink` and `MessageTurn` are
transport-neutral, and a producer registers by implementing three methods. This
is why "buzz is hard to add" is *not* a reason given here — difficulty is low;
demand and operational cost are the problems.

### A live regression was found during this review

Drain phase 2 is dead on both production agents. `waitDeliveries` counts
`pending_turns` rows, but the 2026-08-07 cutover routed writes to the queue
ledger, so the count is now permanently 0 and the barrier returns immediately.
`pending_turns` is frozen at 2026-08-08 01:17 (pika) and 2026-08-07 16:12
(umbreon) while telegram tasks continue to 03:14 and 14:28. This silently
disables the fix for the 2026-08-01 incident where a turn replayed in front of
the family after drain declared idle.

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
2. Is MinIO genuinely required? `ARCHITECTURE.md` says optional,
   `deploy/compose/README.md` says required.
3. Has issue #2663 shipped? It changes integration cost, not the verdict.
4. Would a Go Nostr client (`nbd-wtf/go-nostr`) handle buzz's NIP-42 handshake
   and extended NIP-01 filters? Untested by anyone.
