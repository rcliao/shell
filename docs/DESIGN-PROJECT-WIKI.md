# Project docs as a shared wiki

## 1. Intent
One doc, one page, one owner.

## 2. Decisions
- **Notion state in the doc dir over per-agent rows** — each agent's row binds its own page to the same shared doc, so every shared doc shows up twice in Notion[^E1].
- **Both rows keep the one `page_id` over clearing the non-owner's** — the turn hook, home board, RPC and CLI read the row's `export_ref` directly[^E6].
- **Any writer renders, only the owner polls** over owner-only rendering — the page is current right after any agent's write; one poller means one reply per comment[^E2].
- **The receiving agent takes ownership over a push from outside** — `claimOwner` hands a doc back to whichever agent runs research, so the taker must write its own research flag[^E5].

## 3. Components
- **DocWrite** — doc-write RPC: commits the doc, enqueues a render
- **DocBinding** — `notion.json` beside the shared doc: page and poll state
- **BindingStore** — reads and writes Notion fields from DocBinding for shared docs
- **Renderer** — mirrors the doc's head rev to the bound page
- **NotionPoll** — sweeps comments and edits of owned pages
- **EventHandlers** — reply to a comment; fold a page edit into the doc
- **OwnerFile** — `owner`: which agent polls, researches and summarizes
- **OwnerTake** — `project take <slug>`: the calling agent becomes owner

## 4. Data flow
1. BindingStore (owner) seeds DocBinding from its own row's page and block map the first time it loads a shared doc without one.
2. BindingStore (any agent) overlays DocBinding on the row it loads, and rewrites the row's `export_ref` when it differs from DocBinding's `page_id`.
3. Any agent's DocWrite commits the shared doc and enqueues a render on that agent's queue.
4. Renderer takes `.notion.lock` (waiting out the other daemon's render), updates the bound page, and — only if it reached Notion — records `rendered_rev` and `rendered_at` in DocBinding.
5. NotionPoll (owner only) sweeps the bound page, writes its watermark to DocBinding, and enqueues a render when `rendered_rev` is behind the doc's head (under a fresh key if the render for head failed).
6. EventHandlers (owner) answer a comment and mark its discussion handled in DocBinding.
7. EventHandlers (owner) fold a page edit into the doc only if `rendered_rev` equals the doc's head when the handler runs.
8. OwnerTake (receiving agent) rewrites OwnerFile with its own research flag; its next NotionPoll and research cover the doc.

## 5. Data model
| thing | why |
|---|---|
| `post-<thread>/notion.json` `{page_id, block_map, rendered_rev, rendered_at, watermark, handled}` [NEW] | one binding per doc, readable by both daemons |
| `post-<thread>/.notion.lock` [NEW] | one render or reconcile of the page at a time across daemons; waited on up to 6 min (longer than one render), since the queue retries with no backoff[^E7]; lock order is `.notion.lock` then `.doc.lock` |
| `post-<thread>/.binding.lock` [NEW] | short read-modify-writes of `notion.json` (watermark, handled threads), so a long render never stalls a poll |
| `post-<thread>/owner` `agent\n[research]` [KEEP] | OwnerTake rewrites it |
| row `export_ref` [CHANGE] | equals DocBinding `page_id` in every agent's row |
| row `block_map`, `notion_watermark`, `handled_discussions` | ignored for shared docs; still used for unshared docs |

## 6. Interfaces
- `BindingStore.GetProjectBySlug(slug) → *Project` with DocBinding overlaid · errors: a malformed `notion.json` → returned; the caller skips this render or poll and never rebinds.
- `BindingStore.UpdateProjectFields(slug, u)` → Notion fields to DocBinding, the rest to the row · errors: none beyond the store's.
- Renderer on a shared doc · `.notion.lock` still held after the wait → error; the owner's poll re-queues per step 5. No token → renders nothing and records nothing. No DocBinding and caller is not the owner → `skipped: waiting for the owner's binding` (not an error; the owner's poll catches up per step 5).
- `project take <slug>` (skill, RPC) and `shell project take <slug>` (CLI, as the configured agent) → prints `owner: <agent>` · errors: not a shared doc. Already owner → prints `already owner` (not an error).

## 7. Plan
1. **DocBinding** [NEW] — `project/binding.go`: load and save under `.binding.lock`; `TryLockNotion` for the page.
2. **BindingStore** [NEW] — wraps the store; overlays, seeds and routes Notion fields when `IsShared`.
3. **Renderer** [CHANGE] — persists through BindingStore; takes the lock; records `rendered_rev` and `rendered_at`.
4. **NotionPoll** [CHANGE] — skips shared docs it doesn't own; state through BindingStore; re-renders when behind.
5. **EventHandlers** [CHANGE] — handled set through BindingStore; reconcile gated on `rendered_rev`.
6. **OwnerTake** [NEW] — RPC, skill line, CLI.
7. **DocWrite**, **OwnerFile** [KEEP].

## 8. Done
- Each shared doc has one page · proof: `for a in pikamini umbreonmini; do sqlite3 ~/.shell/agents/$a/shell.db "select message_thread_id, export_ref from projects where status='active' and export_ref != ''"; done | sort -u | cut -d'|' -f1 | uniq -d` → empty · invariant: a thread never has two page ids.
- A non-owner write reaches the owner's page, and only the owner polls · proof: `go test ./internal/project/ ./internal/daemon/ -run 'Binding|Wiki'` → ok (two stores, one shared dir).
- Ownership moves and sticks · proof: `go test ./internal/project/ -run TakeOwner` → ok, including a `claimOwner` pass after the take.
- Live · proof: both `daemon.log` files after deploy show renders of the shared docs and `notion poll: sweep completed … failures=0`.

[^E1]: Five shared docs have two pages · `sqlite3 ~/.shell/agents/{pikamini,umbreonmini}/shell.db "select slug,message_thread_id,export_ref from projects where status='active'"` · 旅遊 (thread 1553785300667605082): pikamini `3eca…ffc1`, umbreonmini `3e8a…2bf80`; the same for gaming/遊戲, 日本 2027, 台灣 2027 and 行李箱 BF. Both rows' `workspace/projects/<slug>` are symlinks to one `~/.shell/shared/projects/post-<thread>/`.
[^E2]: The poll walks each agent's own rows · `internal/daemon/projectnotion.go:121-128` (`ListProjects(0)`, filtered on the row's `export_ref`). A comment on umbreon's copy never reaches pikamini.
[^E3]: Page edits on shared docs are not reconciled today · `internal/daemon/projectnotion.go:500-508`: "reconciling a page that lags the doc would roll back the other agent's changes … Until renders track the doc's rev". `rendered_rev` closes this.
[^E5]: Research follows the owner, and the owner follows research · `internal/daemon/projectevents.go:137` (`MayResearch`); `internal/project/shared.go:112` (`claimOwner`: an agent with research reclaims from an owner without it at every `LinkShared`, which runs at daemon start). All five duplicated docs: `cat ~/.shell/shared/projects/post-*/owner` → `pikamini` + `research`.
[^E7]: No retry backoff · `internal/store/tasks.go` `FailTask` requeues with no delay; a fresh review's repro burned 3 attempts in 1.26 ms against a held lock (`TestWikiFailedRenderCaughtUpByOwnerPoll` now covers the recovery).
[^E6]: Direct readers of the row's `export_ref` · `grep -rn "ExportRef\|NotionPageURL(" internal cmd | grep -v _test` → `bridge/project_hook.go:255`, `project/home.go:188`, `daemon/projectevents.go:178`, `rpc/project.go:499,729`, `cmd/shell/project.go:112`.
