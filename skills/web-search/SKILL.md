---
name: web-search
description: Search the web using Brave/Tavily APIs
usage: scripts/web-search "<query>" [--max N]
allowed-tools: Bash
---

# Web Search

Search the web and get formatted results.

## Usage

```bash
scripts/web-search <query>
scripts/web-search -n 10 <query>
scripts/web-search -f pw <query>
```

## Options

- `-n <count>` — number of results (default 5)
- `-f <freshness>` — time filter: `pd` (24h), `pw` (7d), `pm` (31d), `py` (1yr)

## Examples

```bash
scripts/web-search golang error handling best practices
scripts/web-search -n 3 -f pw latest Claude API updates
```
