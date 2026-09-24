---
name: web-search
description: Place/hours verification contract for web research; prefer built-in WebSearch — this script adds freshness filters when a Brave/Tavily key exists
usage: ~/.shell/skills/web-search/scripts/web-search "<query>" [-n N] [-f pd|pw|pm|py]
allowed-tools: Bash
---

# Web Search

**Default: use the built-in `WebSearch` tool** (and `WebFetch` to read a page).
It needs no key and is the reliable path.

Use this script only when you need what it adds — a freshness filter (`-f`)
and each result's age — which requires a Brave or Tavily key. Without one it
falls back to DuckDuckGo, which often refuses automated requests. When that
happens the script prints **`SEARCH UNAVAILABLE`** and exits non-zero: that
means the search did not run, NOT that nothing exists. Switch to `WebSearch`;
never answer "I searched and found nothing" from it.

## Usage

```bash
~/.shell/skills/web-search/scripts/web-search "<query>" -n 10 -f pw
```

## Options

- `-n <count>` — number of results (default 5)
- `-f <freshness>` — time filter: `pd` (24h), `pw` (7d), `pm` (31d), `py` (1yr)

For anything about CURRENT status — open/closed, hours, prices, schedules,
availability — pass `-f py` (or tighter) and weigh the `Age:` line on each
result. An undated listicle can be years stale no matter how well it ranks.

## Location & place research contract

Real incident (7/27): two ice-cream shops were recommended from top-ranked
travel listicles; both had permanently closed. Listicles never say "closed."
A search hit naming a place is NOT evidence the place exists today.

For EVERY specific place you recommend or plan around (restaurant, shop,
bakery, attraction):

1. **Verify current status per business** before asserting — use the browser
   skill on the business's official site, or its Yelp / Google Maps listing
   (these show "Permanently closed" and current hours). One search + one
   verification per place; spend the extra tool calls — a wrong "it's open"
   costs the family a trip.
2. **Hours claims need a source that shows hours** — never state opening
   hours from a listicle or blog snippet.
3. **Date your claims**: "as of my check today (<date>)" — and if you could
   not verify a place, say so explicitly instead of sounding confident.
4. This applies to anything you WRITE DOWN too (Notion itineraries, docs):
   persisted hours/status inherit the verification of the turn that wrote
   them — verify before persisting, and note the verified-on date in the doc.
