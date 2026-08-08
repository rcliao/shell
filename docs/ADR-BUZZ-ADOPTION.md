# ADR: how much of Buzz to adopt

Status: proposed · Date: 2026-08-08

## Context and Problem Statement

Buzz is a Nostr-based workspace where agents are members with their own
keypairs. Two shell agents were run against a hosted relay for a day: attested
identity, engram memory, mentions, threads, reactions, git issues, and two
agents delegating to each other unattended all worked.

The question is not whether Buzz works. It is which of four layers, if any,
shell should adopt: the **transport** (relay instead of Telegram), the **agent
runtime** (`buzz-acp` instead of shell's bridge), the **identity model**
(Nostr keypairs plus owner attestation), or the **storage primitives**
(engrams with compare-and-swap).

What forces the choice now: Buzz's own spec says the launcher is pluggable, so
shell already qualifies as a conforming launcher with no code change — adoption
is cheap enough that "no" needs a reason. Against that, thirty days of usage
show 2,183 family-group turns, 400 DM turns to the mother, **4** DM turns to
the father, 586 inbound photos, and 21 live schedules. The traffic is one
household, on phones, with heavy media and scheduled push.

## Considered Options

### A. Replace Telegram with Buzz

Pros: one identity model, searchable history, agents and humans as peers.

The media path is no longer untested: an owner-posted image round-tripped
correctly. Blossom storage is content-addressed (the filename is the sha256,
which verified byte-for-byte on download) and auth-gated (401 unauthenticated)
— both stronger than shell's flat `~/.shell/media` archive, which has no
checksum. But the agent only *saw* it by shelling out to `buzz media get` and
reading the file; the harness never passed it as an ACP image block despite
advertising `promptCapabilities: {image: true}`. Shell passes typed `ImageInfo`
into the turn with no incantation.

Cons: the mother would have to move to a new mobile app; buzz's shipping
client showed no push notification path, and push is the entire delivery
mechanism for the 21 schedules. 586 photos in five weeks would move to a media
path that works but needs the agent to know a CLI call. Relay-side outage becomes a family outage. No migration
path for ~6 GB of existing transcripts, and signing old history with keys that
did not exist then would forge the audit trail the migration was for.

### B. Add Buzz as a second transport

Pros: cheap — `message.turn` intake is already transport-neutral, and a
producer needs only `messages get --since` polling.

Cons: no user. The father already reaches both agents via Telegram and
`shell chat`; his DM traffic is 4 turns in 30 days. Nostr events are immutable,
so `Sink.Update` must be a no-op and streaming is lost.

### C. Replace shell's bridge with `buzz-acp`

Pros: maintained by someone else; mid-turn steering, dedup, presence, memory
injection already built.

Cons: it is a peer of shell's bridge, not an upgrade — the same feature list,
minus ExecutionProfile, model/effort routing, rotation, ghost, write-hygiene
and the Telegram-specific tuning. Trading a tuned implementation for an
equivalent untuned one.

### D. Adopt the identity *primitive* only

Pros: NIP-OA is ~90 lines of spec over BIP-340; a signer was implemented in an
afternoon with no dependencies. Gives agents cryptographic identity, revocable
authority, verifiable provenance, and a spoof-proof id for a2a routing — with
Telegram as transport and no relay at all.

Cons: provenance nobody verifies is inert while the family is the only
audience. Adds key management with no present consumer.

Storage was originally listed here on the belief that engram CAS answered
ghost's last-writer-wins. **That was false.** Ghost already has
compare-and-swap at every layer — `ErrVersionConflict` in the store,
`--base-version` on `put`/`patch`, and `base_version` on the agent-facing
`ghost_put` and `ghost_patch` MCP tools. Buzz's engram CAS is the same idea,
keyed on a content hash instead of a monotonic version.

### E. Adopt Buzz's shared docs (notes + canvas)

This is the one capability shell genuinely lacks, and the reason it was
considered: Telegram has no documents, so docs live in Google/Notion today,
disconnected from the conversation.

Pros: verified working. An agent updated a shared note from chat and *merged*
rather than overwrote — prior sections survived. Notes are slug-addressable
with a stable `naddr`; both agents read each other's. Canvas is a per-channel
living document. Both sit beside the conversation with nothing to integrate.

Cons, and they decide it: **editing is a raw markdown input**. For the
non-technical daily user who currently keeps docs in Google/Notion, that is a
downgrade in the only interaction that matters — hers. Neither notes nor canvas
has revision history, and neither has a write guard, so a concurrent edit is
lost silently (only `mem` has compare-and-swap).

Cheaper alternative: extend shell's existing Google/Notion skill to format and
edit better. That keeps her in an editor she already uses.

## Decision Outcome

**Adopt none of the five. Revisit only on a demand signal.**

The decision changed while writing this. The draft recommended importing engram
CAS into ghost; checking the code showed ghost has had compare-and-swap all
along, including on the tool agents actually call. With that gone, no layer has
a present consumer: transport loses on measured usage, the runtime is a peer
rather than an upgrade, and attested identity is real but has no verifier while
the household is the only audience.

The runner-up is E. Shared docs are the one capability shell lacks outright,
and the agent-maintenance half works well — but the editing surface is markdown
in a textarea, and the person who would use it most is the one it suits least.
Improving shell's Google/Notion skill is the cheaper path to the same outcome.

Three further tests changed nothing but are worth recording. Session continuity
is *not* capped at `context_limit=12` — a fact survived 15 intervening messages
because the ACP session persists, so the hypothesis that Buzz would degrade on
long-running context was wrong. Proactive delivery works via
`--heartbeat-interval`, but expresses only a fixed global interval where shell
has 21 per-chat schedules with timezones and dated one-shots. And Buzz forums
are votable threaded posts (`kind:45001`/`45003`), not containers — they are
not equivalents of the five Telegram topics in use.

## Consequences

Negative: shell keeps carrying its own bridge, including the absorb race and
the drain-barrier class of bug, where `buzz-acp` would have been someone else's
maintenance. Declining the identity model means a2a routing stays name-based,
which has already caused one silent-failure class. Revisiting later costs a
re-evaluation because Buzz moves fast.

Positive: no new services, no key rotation policy, no second messaging client
for a non-technical daily user, and the family's message path stays on
infrastructure that demonstrably delivers push.

Also positive, and the real return on this evaluation: it surfaced a live
drain-barrier regression and confirmed that shell's bridge, coalescing,
steering and CAS choices independently match what a well-resourced team built.

One primitive is worth stealing without adopting anything: content-addressed
media. A sha256 column on shell's media ledger would make a truncated or
substituted photo detectable, which today nothing would notice.

Revisit when: a second household machine, a third agent, or an outside
collaborator needs to reach these agents — that is when relay-side services and
verifiable provenance stop being inert. Buzz maturity is not the trigger;
demand is.
