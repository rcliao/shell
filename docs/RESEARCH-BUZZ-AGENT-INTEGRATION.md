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

`docs/remote-agents.md` §Launchers is explicit: a live agent is a `buzz-acp`
process holding a keypair, an auth tag, and a relay URL as environment, and
"the relay authenticates the keypair and the auth tag — never the launcher."
A bash script that exports those three and execs the harness is conforming
"today, with no code change." The desktop is "one launcher among many."

### The desktop agent list is a deployment registry, not an identity roster

A self-launched agent does not appear in the desktop's agents list, and this is
by construction, not misconfiguration: hand-launched agents "sit outside" the
provider contract, and the desktop "holds no management channel to the remote
process." Presence is the only status signal. So an agent can be fully live,
posting under its own key, and still be absent from that list.

### The attestation is load-bearing at runtime, not just for writes

With `BUZZ_AUTH_TAG` set, the harness logs `owner resolved from BUZZ_AUTH_TAG`
and uses it to gate `--respond-to=owner-only`. Without it there is no owner
concept at all. Separately, an attested agent writes memory with no `--owner`
flag — the namespace derives from the credential. Self-attestation is rejected
by spec.

### The harness is a peer of shell's bridge, not a layer above it

Startup config reads `subscribe=Mentions dedup=Queue meh=Steer
ignore_self=true context_limit=12 max_turns_per_session presence typing memory
permission_mode respond_to`. That is shell's feature list — including mid-turn
steering, which shell reached independently. They are alternative
implementations of the same role, and convergent design is evidence both are
reasonable.

### Verified working; one real defect, one retracted

Round trip confirmed: mention → relay → harness → ACP session → reply under the
agent key, engram injected automatically. Two agents then delegated to each
other unattended.

Real defect: workflow runs never materialise — `trigger` returns a `run_id`,
`runs` returns `[]`, and no `invoke_agent` action exists.

**Retracted:** an earlier draft reported `--respond-to allowlist` dropping
mentions silently. At `RUST_LOG=debug` it admits them normally. The apparent
drop was `agent_claimed` logging at DEBUG plus `plan` mode blocking the reply.

Log-level filtering caused two false findings here. Claims need DEBUG logs or
CLI output, never absence of log lines.

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
3. Does a steered turn end, or absorb later mentions indefinitely? The
   7200s deadline extension is logged; turn closure was not observed because
   `pool::prompt` was filtered out.
