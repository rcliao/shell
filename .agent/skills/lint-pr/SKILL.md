---
name: lint-pr
description: Lint staged or PR changes against project conventions
allowed-tools: Bash
---

# Lint PR

```bash
scripts/lint-pr
```

Runs `go vet`, checks for common issues in changed files, and reports findings. Uses `git diff --cached` for staged changes or `git diff main...HEAD` for branch changes.
