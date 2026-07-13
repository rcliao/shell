---
name: notion
description: Read and write Notion pages/databases via lightweight REST scripts — get-page, query-db (with date filter), patch-prop (with read-back receipt), append
allowed-tools: Bash(scripts/notion:*)
tier: hot
---

# Notion (lightweight REST)

Use `scripts/notion` for ALL Notion operations. There is no Notion MCP server.

```
scripts/notion get-page <page_id>                       # read a page's properties
scripts/notion query-db <db_id> [--date <prop>=<ISO>]   # list rows, optional date filter
scripts/notion patch-prop <page_id> <prop> <value>      # write ONE property; prints read-back
scripts/notion append <page_id> <text>                  # append a paragraph block
```

Rules:
- **The read-back line printed by patch-prop is your proof of persistence.** Only claim
  "saved/記下了" after seeing it, and check it matches what you meant to write.
- Structured data (meal logs, itineraries) goes in DB property COLUMNS via patch-prop,
  never page-body paragraphs.
- Database/page IDs come from your memory or the conversation — this skill stores none.
