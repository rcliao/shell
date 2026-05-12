---
name: browser
description: Automate headless Chrome — navigate, click, type, extract, screenshot
usage: scripts/browser <subcommand> [args]  # e.g. screenshot URL OUT.png
allowed-tools: Bash
---

# Browser Automation

Automate a headless Chrome browser for web interaction tasks.

## Usage

```bash
scripts/browser <url> [action...]
```

Each action is a separate argument. Available actions:

- `screenshot` — capture full page screenshot
- `click "<selector>"` — click an element by CSS selector
- `type "<selector>" "<value>"` — clear and type into an input
- `wait "<selector>"` — wait for element to appear (up to 10s)
- `extract "<selector>"` — extract text content of element(s)
- `js "<expression>"` — evaluate JavaScript and return result
- `sleep "<duration>"` — wait (e.g., `sleep "2s"`)

## Examples

Take a screenshot:
```bash
scripts/browser "https://example.com" screenshot
```

Multi-step interaction:
```bash
scripts/browser "https://example.com/login" \
  'type "#email" "user@example.com"' \
  'type "#password" "secret"' \
  'click "#submit"' \
  'wait "#dashboard"' \
  screenshot \
  'extract "#welcome-message"'
```

Extract page content:
```bash
scripts/browser "https://example.com" 'extract "body"'
```

## Output

- Text results (extract, js, click, etc.) are printed to stdout
- Screenshots are saved to temp files and output as artifact markers:
  ```
  [artifact type="image" path="/tmp/shell-browser-123.png" caption="Screenshot of https://..."]
  ```
- You MUST include artifact markers verbatim in your response so the bridge delivers images to the user
- Errors are printed to stderr

## Environment

- `CHROME_PATH` — custom Chrome binary path (optional)
- `BROWSER_HEADLESS` — set to `false` to run with visible browser (default: headless)
