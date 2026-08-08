# Research: running shell's agents as Buzz agents

## Research Question

What does it actually take to run an existing agent (own runtime, own session
state) as a first-class Buzz agent, and what does Buzz's agent layer provide
that shell does not already have? Tested 2026-08-08 against a hosted relay with
two self-generated identities and a live ACP harness.

## Summary

A live Buzz agent is a process holding three things — a keypair, a NIP-OA owner
attestation, and a relay URL — handed to the `buzz-acp` harness. There is no
registration step and no agent registry to join. A verified round trip ran end
to end: one agent mentioned another, the relay pushed it, the harness woke a
Claude session, and the reply posted under the agent's own key.

The agent layer's real contribution is identity: attested authorship, an owner
relationship the harness resolves at runtime, and relay-hosted memory injected
automatically. Its orchestration layer does not work. Shell qualifies as a
"conforming launcher" today, without code changes.

## Findings

### A live agent is environment plus harness — there is nothing to register

`docs/remote-agents.md` §Launchers: a live agent is a `buzz-acp` process
holding a keypair, an auth tag and a relay URL as environment, and "the relay
authenticates the keypair and the auth tag — never the launcher." A bash script
exporting those three is conforming "today, with no code change".

### The desktop agent list is a deployment registry, not an identity roster

A self-launched agent never appears in the desktop's agents list, by
construction: hand-launched agents "sit outside" the provider contract and the
desktop "holds no management channel to the remote process". Presence is the
only status signal. Fully live and absent from that list are not in conflict.

### The attestation is load-bearing at runtime, not just for writes

With `BUZZ_AUTH_TAG` set the harness logs `owner resolved from BUZZ_AUTH_TAG`
and gates `--respond-to=owner-only` on it; without it there is no owner concept
at all. An attested agent also writes memory with no `--owner` flag — the
namespace derives from the credential. Self-attestation is rejected by spec.

### The harness is a peer of shell's bridge, not a layer above it

Startup config reads `subscribe=Mentions dedup=Queue meh=Steer ignore_self
context_limit max_turns_per_session presence typing memory permission_mode
respond_to` — shell's feature list, including mid-turn steering, which shell
reached independently. Convergent design on the same role.

### Verified working; one real defect, one retracted

Round trip confirmed: mention → relay → harness → ACP session → reply under the
agent key, engram injected. Two agents then delegated unattended.

Real defect: workflow runs never materialise — `trigger` returns a `run_id`,
`runs` returns `[]`, and no `invoke_agent` action exists.

**Retracted:** an earlier draft reported `--respond-to allowlist` dropping
mentions silently. At `RUST_LOG=debug` it admits them normally. The apparent
drop was `agent_claimed` logging at DEBUG plus `plan` mode blocking the reply.

Log-level filtering caused two false findings here. Claims need DEBUG logs or
CLI output, never absence of log lines.

### Memory: autonomous, conflict-safe — but neither is a platform property

Asked to remember a fact, the agent chose a slug, wrote the engram, and read it
back unprompted — the read-back discipline shell had to enforce in
`write_verify.go`. One observation, not a pattern.

CAS is specific to engrams. `canvas set` has no `--base-hash` and is
last-writer-wins: two agents wrote, the second won silently. `dms open` yields
a private channel of kind:9, not NIP-17 gift-wrapped kind:1059 — privacy is
relay ACL, so the operator can read it.

Provenance is client-enforced: NIP-OA says "relays MUST NOT be required to
verify an `auth` tag". The CLI does verify, refusing to publish a tampered one.
A scoped credential constrains verifiers, not the agent's power, which comes
from relay membership.

## Code References

- `docs/remote-agents.md` §Launchers — the three nested launcher contracts
- `crates/buzz-acp/README.md` — keypair + `add-member` onboarding
- `docs/nips/NIP-OA.md` — attestation preimage and conditions grammar
- `docs/nips/NIP-AA.md` — virtual membership via owner credential

## Open Questions

1. Does any launcher-agnostic way exist to surface a hand-launched agent in
   the desktop UI, or is presence the only signal available?
2. Would scoping the attestation (`kind=30174`) break the harness's owner
   resolution, which appears to read the tag regardless of conditions?
3. Does a steered turn end, or absorb mentions indefinitely? The 7200s
   deadline extension is logged; closure was never observed.
4. Does the relay verify `auth` tags, or only the CLI? Probing it needs a raw
   Nostr client.
