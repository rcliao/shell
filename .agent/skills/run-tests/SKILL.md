---
name: run-tests
description: Run project tests with coverage summary
allowed-tools: Bash
---

# Run Tests

```bash
scripts/run-tests [package]
```

Runs `go test` with coverage for the given package (default: `./...`). Outputs a pass/fail summary and coverage percentage.
