---
name: meal-memo
description: Standardized meal memo format used 25+ times/week with mami — bullets, dairy total, water mention
allowed-tools: Read
status: draft
draft_cycle: 2
---

# Meal Memo Format

When mami logs a meal, reply in this format. Derived from the pattern pikamini already uses
~25× per week (last 200 msgs: 42 dairy mentions, 25 dinner memos, 32 water mentions).

## Format

```
收到 mami～ 📝⚡

**[meal-emoji] M/D (週X) [meal-name]：**
▫️ [item 1 with emoji]
▫️ [item 2 with emoji]
▫️ ...

---

**🥛 Dairy：~X.X pt** [✅ if low, ⚠️ if moderate, ❌ if high]
**💧 Water reminder if no liquid logged**

**📊 [day] dairy 全日總計：~X.X pt** [✅ 安全 / ⚠️ 注意 / ❌ 超標]
```

## Rules

1. **Always use ▫️ bullets**, never markdown tables (per family convention).
2. **Always include dairy estimate in pt** even if 0 (write `0 pt ✅`).
3. **Carry forward earlier same-day dairy** — only sum within the same day.
4. **Match mami's language** — Chinese meal description → Chinese reply.
5. If mami says "跟昨天一樣", **copy ALL items from yesterday's memo**, not just the last one.
6. Only log medications when mami confirms taken — suggestion ≠ taken.

## When NOT to apply

- Snack notes that mami sends as side-comments rather than a logged meal.
- Restaurant research / pre-meal planning (different format).
- Dairy *summary* requests for the week — use a table-ish vertical list.

## Test cases

- `5/6 晚餐：地瓜 + 水果 + 優格` → standard format
- `跟昨天一樣` → copy all yesterday items, don't truncate
- `今天午餐 yellow curry chips` → snack note, just acknowledge with dairy estimate
