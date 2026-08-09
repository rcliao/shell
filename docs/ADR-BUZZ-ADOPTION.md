# ADR: how much of Buzz to adopt

Status: **accepted** (owner, 2026-08-08) · Date: 2026-08-08

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

Media works: an owner-posted image round-tripped, and Blossom storage is
content-addressed (sha256 verified byte-for-byte) and auth-gated — both
stronger than shell's flat archive. But the agent only *saw* it by shelling out
to `buzz media get`; the harness never passed it as an ACP image block.

Cons: the mother would move to a new mobile app; buzz's shipping client showed
no push path, and push is the entire delivery mechanism for the 21 schedules.
586 photos in five weeks would move to a media path needing a CLI call. Relay-side outage becomes a family outage. No migration
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

Pros: verified. An agent updated a shared note from chat and *merged* rather
than overwrote. Notes are slug-addressable; both agents read each other's.
Canvas is a per-channel living document.

Cons, and they decide it: **editing is a raw markdown input** — a downgrade in
the only interaction that matters, hers. No revision history, and no write
guard on either (only `mem` has compare-and-swap). Extending shell's
Google/Notion skill is the cheaper path to the same outcome.

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

Three tests changed nothing but corrected me, and are detailed in
`RESEARCH-BUZZ-AGENT-INTEGRATION.md`: continuity is *not* capped at
`context_limit=12` (the ACP session persists); proactive works but expresses one
global interval against shell's 21 scheduled jobs; forums are votable threaded
posts, not topic containers.

**Owner's framing, accepted:** Buzz is not mature enough to switch to, but it
is a source of design inspiration for shell.

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

Follow-ups this evaluation earned, in priority order:

1. **ACP-shaped process boundary.** `buzz-acp` talks to any agent over a
   standard stdio protocol, so goose, codex and Claude are interchangeable.
   Shell's bridge↔process boundary is bespoke. Adopting that shape would make
   the agent runtime swappable and testable without a live model.
2. **A2A on stable ids.** Buzz routes by pubkey in a `p` tag — unspoofable, and
   threads are addressable units. Shell matches names and keeps delegation in a
   shared store, which is the `[relay]`-directive failure class.
3. **A canvas per Telegram topic.** The one capability shell lacks outright: a
   living document bound to a conversation. Buzz's editing surface is unusable
   for the primary user, so the move is shell's own doc skill, not Buzz's store.
4. **Content-addressed media.** A sha256 column on the media ledger makes a
   truncated or substituted photo detectable; today nothing would notice.

Revisit when: a second household machine, a third agent, or an outside
collaborator needs to reach these agents — that is when relay-side services and
verifiable provenance stop being inert. Buzz maturity is not the trigger;
demand is.
