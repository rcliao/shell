# PII gate: run it, and never through a pipe

`make verify-no-pii` must pass before **every** commit in this repo.

## Never pipe it

```bash
make verify-no-pii | tail -1 && git commit ...   # WRONG
make verify-no-pii && git commit ...             # right
```

A pipeline's exit status is the **last** command's. `make verify-no-pii | tail`
returns `tail`'s status, which is 0 whether the gate passed or failed, so the
`&&` fires and the commit lands anyway. The output still says something
reassuring — `tail` prints the last line either way — so it *looks* verified.

This is not hypothetical. On 2026-08-06 that exact pipeline masked a failure
and committed a real Telegram chat ID into a tracked test file. It was caught
before pushing only because the commit was re-read by hand.

If you want to see just the summary, run it, then look:

```bash
make verify-no-pii || exit 1
```

## What it protects

No real chat IDs, usernames, personal names, or verbatim quotes from family
conversations in tracked files — source, tests, docs, commit messages. Use:

- synthetic IDs in tests (`-100200300`, `42`), never a real chat id
- "the user", "the family", "a forum group" in comments and docs
- reviews go to `~/.shell/evolve-reviews/`, aliases to `~/.shell/eval-aliases.txt`

Real identifiers belong in `~/.shell/`, which is not a git repository.

## Staging matters

The gate checks **staged** files. `git add` first, then run it — an unstaged
change is invisible to it and it will say "nothing staged", which is not the
same as "clean".
