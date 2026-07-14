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
scripts/notion append <page_id> <text>                  # append a paragraph at page END
scripts/notion append <page_id> --after <block_id> <text>  # insert AFTER a specific block
scripts/notion list-blocks <page_id>                    # block ids + text (find insert/edit targets)
scripts/notion update-block <block_id> <text>           # replace an existing block's text (read-back)
scripts/notion delete-block <block_id>                  # archive a block
```

Rules:
- **The read-back line printed by patch-prop is your proof of persistence.** Only claim
  "saved/記下了" after seeing it, and check it matches what you meant to write.
- Structured data (meal logs, itineraries) goes in DB property COLUMNS via patch-prop,
  never page-body paragraphs.
- To place content under a SPECIFIC section (not page end): list-blocks first, find the
  heading's block id, then append --after that id. Plain append lands at the page bottom
  — do not claim content is "under section X" unless you inserted after X's block.
- Database/page IDs come from your memory or the conversation — this skill stores none.
