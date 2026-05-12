---
name: summarize
description: Summarize a file or URL in a few bullet points
usage: scripts/summarize <path-or-url>
---

# Summarize

When asked to summarize, produce a concise bullet-point summary (3-5 points). Focus on key takeaways, not details.

## Usage

- For files: use the Read tool (already approved) to read the file, then summarize
- For URLs: use WebFetch or the `browser` skill script to fetch content, then summarize
- Do NOT use Bash to read files — use the Read tool instead
