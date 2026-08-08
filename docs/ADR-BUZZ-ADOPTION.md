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

Cons: the mother would have to move to a new mobile app; buzz's shipping
client showed no push notification path, and push is the entire delivery
mechanism for the 21 schedules. 586 photos in five weeks would move to an
untested media path. Relay-side outage becomes a family outage. No migration
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

### D. Adopt the identity and storage *primitives* only

Pros: NIP-OA is ~90 lines of spec over BIP-340; a signer was implemented in an
afternoon with no dependencies. Gives agents cryptographic identity, revocable
authority, verifiable provenance, and a spoof-proof id for a2a routing — with
Telegram as transport and no relay at all. Engram CAS (`hash` → `patch
--base-hash` → conflict) is a direct answer to ghost's last-writer-wins.

Cons: provenance nobody verifies is inert while the family is the only
audience. Adds a key-management burden with no present consumer.

## Decision Outcome

**Take D's storage primitive now; keep A, B and C rejected; hold D's identity
half until a verifier exists.**

Ghost gains compare-and-swap: a content hash on read, a guarded write, a
distinct conflict outcome. That beats the runner-up (D-in-full) because CAS has
a consumer today — two turns editing one memory silently clobber each other —
while attested identity has none until someone outside the household needs to
check who authorized an agent.

## Consequences

Negative: shell keeps carrying its own bridge, including the absorb race and
the drain-barrier class of bug, where `buzz-acp` would have been someone else's
maintenance. Declining the identity model means a2a routing stays name-based,
which has already caused one silent-failure class. Revisiting later costs a
re-evaluation because Buzz moves fast.

Positive: no new services, no key rotation policy, no second messaging client
for a non-technical daily user, and the family's message path stays on
infrastructure that demonstrably delivers push.

Revisit when: a second household machine, a third agent, or an outside
collaborator needs to reach these agents — that is when relay-side services and
verifiable provenance stop being inert. Buzz maturity is not the trigger;
demand is.
