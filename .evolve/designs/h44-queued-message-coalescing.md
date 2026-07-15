# V2-H44 — Queued-message coalescing (design for owner review)

**Problem (7/15 morning):** owner-A sent 3+ messages while a 117s memo turn ran.
Serialization made each queued message wait its full predecessors (lock waits
52s / 136s / 65s; worst message experienced ~175s end-to-end for a short
follow-up). Everything was answered, but as three sequential slow turns.

**Proposal:** when a turn finishes and MORE THAN ONE message from the same
human is waiting on that (chat, thread) lock, merge the waiters into ONE turn.

## Mechanics
1. Per (chat, thread), a small waiting-room: handler enqueues {msgID, text,
   placeholder-ID, timestamps} instead of blocking on the chat mutex when the
   lock is held AND the queue is non-empty.
2. When the lock frees, the drainer takes ALL queued entries from the same
   sender and builds one user message:
      [She sent several messages while you were working:]
      1. <text 1>
      2. <text 2> ...
   Model answers once, addressing each point.
3. Placeholders: keep the FIRST message's placeholder for streaming; the
   later placeholders get edited to "↑ 一起回覆在上面" (or deleted).
4. pending_turns ledger: all coalesced msgIDs marked done by the one turn.
5. Caps: coalesce at most 5 messages; never across senders; never across
   threads; photos/media break the batch (their turns stay individual).

## Non-goals / risks
- Group chat: BOTH agents coalesce independently — fine (same rules).
- Ordering: replies reference numbered points, so no ambiguity.
- Risk: user expects per-message replies (e.g. two unrelated questions) —
  the numbered-answer format covers it; if it feels wrong in practice, the
  cap can drop to 2-3 or the feature can gate on message similarity.
- Kill switch: config `daemon.coalesce_disabled`.

## Measure-by
- lock_wait_ms p95 on multi-message bursts (baseline 7/15: 52-136s)
- owner complaints about missed/merged answers (should be zero)

**Estimated size:** handler queue + drainer ~120 lines, tests for the
waiting-room merge logic. One deploy.
