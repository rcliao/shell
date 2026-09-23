# Design: secrets v2 — owned, headless, scoped

2026-09-23. Owner-approved direction: keep an encrypted store we control,
drop the macOS Keychain dependency, and stop handing every secret to every
subprocess. Evidence appendix at the end.

## Intent

Shell's agents need a handful of credentials (bot tokens, Notion, Gemini,
the Jev key) available to a daemon that runs unattended. Today the store's
master key lives in the login Keychain, which cannot be read from any
non-interactive context (launchd, remote shells, this session — `security`
exit 36), fails with one misleading message for every cause, and whose
suggested remedy (`init`) silently orphans the store. Separately, the daemon
copies **every** stored secret into its own environment, and every Claude
CLI subprocess inherits it: any secret is one `env` away from a
prompt-injected Bash call in a family chat.

Two goals, in order of value:

1. **Scoping** — a secret reaches only the component that needs it.
2. **Headless, owned storage** — decryptable wherever the identity file is,
   no prompts, no OS-specific helper, diagnosable in one command.

Non-goals: a hosted manager as the primary store (1Password stays possible
as a later `exec` backend, per the harness convention); a passphrase agent;
RPC-fetched secrets for skill scripts.

## Components

| Component | Repo | Change |
|---|---|---|
| `shell-secrets` store | shell-secrets | v2 file format encrypted with **age** to a local identity; Keychain path removed |
| `shell-secrets` CLI | shell-secrets | `init` refuses to orphan; `migrate`; `doctor`; typed load errors |
| `config.Secret` / store open | shell `internal/config` | unchanged interface; `ExportSecrets` deleted |
| child environment | shell `internal/process` | strip every store-managed name from children, except the passthrough list |
| `secrets.passthrough` | shell config | names skill binaries may see (default: `GEMINI_API_KEY`, `BRAVE_SEARCH_API_KEY`, `TAVILY_API_KEY`) |
| `shell secrets doctor` | shell CLI | resolves every referenced secret, reports source and gaps |

## Data flow

**Startup.** The daemon reads `~/.shell-secrets/identity` (age key file,
dir 0700 / file 0600, refused if looser), decrypts `secrets.enc` into
memory, and resolves each configured reference — `telegram.token_env`,
`notion.token_secret`, the Jev key — store first, then environment (the
fallback keeps the migration reversible). It hands each value to its
consumer: the Telegram client in-process; the Jev key to the decider's
resolver; the Notion token to the project Notion client in-process (and to
the Notion MCP server's env when that server is enabled — it is off today).
Nothing is written into `os.Environ`.

**Spawning a Claude CLI.** The process manager builds the child env from
`os.Environ()` minus every name the store manages, plus the passthrough
names with their values. Bot tokens and the Jev key never appear in a child
env. **`NOTION_TOKEN` does pass through** (review finding, 2026-09-23): the
notion skill is a Bash script and the agent's only Notion consumer, so a
secret a Bash skill needs is visible to the agent by construction; scoping
it would only turn Notion off. The planner's subprocesses (claude runs,
test and verify shells — Bash-capable) use the same policy. The policy is
computed once at daemon start: a secret `set` afterwards is neither
stripped nor passed through until a restart. Names listed in `claude.env`
are an operator opt-in and bypass stripping.

**Operator.** `shell-secrets set NAME --stdin` writes v2. `shell-secrets
doctor` prints: identity present and permissions OK; store version;
decrypts; N keys; v1 residue present or not. `shell secrets doctor` adds:
for each configured reference, resolved from store / env / **missing**.

**Migration (one-time).** `shell-secrets migrate --from-v1` reads the v1
store (this is the last operation that needs the Keychain, so it runs from
a Terminal window), writes v2, and renames v1 to `secrets.enc.v1-<date>`.
`migrate --from-env NAME...` is the fallback if the Keychain never
cooperates: the daemon's launch environment already carries the four keys.
Because exported variables survive in-place restarts, scoping becomes real
only after a **cold** restart — which v2 makes possible from an
unsandboxed shell, since no prompt is involved.

## Data model

`~/.shell-secrets/secrets.enc`:

```json
{"version": 2, "recipient": "age1…", "payload": "<age armored ciphertext of {\"KEY\":\"value\",…}>"}
```

`version: 1` (nonce + AES-GCM ciphertext) is recognised and refused with a
typed error naming `migrate`. `~/.shell-secrets/identity` is a standard
`age-keygen` file; `SHELL_SECRETS_IDENTITY_FILE` overrides its location.

Shell config gains:

```json
"secrets": {"enabled": true, "passthrough": ["GEMINI_API_KEY", "BRAVE_SEARCH_API_KEY", "TAVILY_API_KEY"]}
```

## Interfaces

- `secrets.Store` (`Get/Set/List/Remove/Close`) unchanged. `WithMasterKey`
  becomes `WithIdentity(age.Identity)` (tests).
- `keychain.Load` errors become typed: exit 44 → not found; exit 36 → "the
  keychain cannot prompt from here — open a Terminal window, run
  `security unlock-keychain`, retry; do NOT run init"; else the real text.
  Kept only for `migrate --from-v1`.
- CLI: `init [--force]`, `set`, `get`, `list`, `rm`, `migrate --from-v1 |
  --from-env NAME...`, `doctor`.
- Shell: `config.ExportSecrets` removed; `config.ManagedSecretNames()` for
  the process manager; `shell secrets doctor`.

## Plan

1. shell-secrets v2 (age store, typed errors, `init` guard, `migrate`,
   `doctor`, tests — the repo has none today); README/CLAUDE.md updated.
2. shell: delete export, add child stripping + passthrough, `shell secrets
   doctor`, `go mod tidy`, ARCHITECTURE.md section.
3. Fresh-context review of both diffs (bot-token path).
4. Deploy: build → owner runs `migrate` → `doctor` → SIGHUP (env fallback
   still holds) → verify `store opened version=2` → cold restart → verify a
   child env lacks every bot/Notion name and carries the passthrough three
   → one web-search turn via `shell chat` still works.

*Verify:* the child-env check above is the acceptance test; `doctor` on
both machines' agents; no `secrets:` warnings in daemon.log.

## Evidence

- `security find-generic-password … -w`: exit 44 = item missing, exit 36 =
  interaction not allowed; `shell-secrets` reports both as "master key not
  found (run init)". `init` deletes the existing key unconditionally
  (`internal/cli/init.go`); `secrets.enc.orphaned-20260824` exists.
- `config.ExportSecrets()` (`internal/config/config.go:637`) sets every store
  key in `os.Environ`; `daemon.go:104` calls it at startup; children get
  `os.Environ()` (`process/manager.go`, `persistent.go`). Cold-start log:
  `secrets: exported to env count=4`. Daemon env today names
  `TELEGRAM_BOT_TOKEN`, `UMBREONMINI_BOT_TOKEN`, `NOTION_TOKEN`,
  `GEMINI_API_KEY`.
- Exec-in-place restart (`syscall.Exec(binary, os.Args, os.Environ())`)
  carries exported vars across every SIGHUP.
- `op` CLI on this machine hangs on every command including `--version`
  (stale `op-daemon.sock` from Feb 15, no daemon running) — same failure
  class: helper process + socket + OS state.
- Harness convention (Hermes, OpenClaw): references in config, eager
  resolution into memory, explicit passthrough allowlist for children,
  provider credentials never pass through, an `audit`/`doctor` command.
