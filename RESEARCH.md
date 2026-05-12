# Shell + Ghost Improvement Research

**Date:** 2026-03-22
**Goal:** Identify highest-impact skills and improvements based on actual usage patterns

---

## Round 1: Architecture Deep Dive

### Research Questions
1. How much context does each heartbeat load, and what's the ghost overhead?
2. What does the current multi-agent implementation look like post-commit 6bc781a?
3. What skills exist today and what gaps are there?
4. What are the most common failure modes in the bridge?
5. How does the scheduler/store handle data lifecycle?

---

### Finding 1: Heartbeat Token Overhead

**System prompt per message: ~2,500-5,000 tokens**
- Agent identity: ~50-150 tokens
- Memory (ghost SystemPrompt): up to 3,000 token budget
- Timestamp: ~30 tokens
- Skills instructions: ~500-2,000 tokens (14 skills loaded)

**Heartbeat enrichment adds ~2,000 more tokens:**
- Recent history: 10 exchanges x 200-300 chars = ~625 tokens
- Heartbeat insights: 500 token budget
- Memory context: hard-capped at 1,000 chars (~250 tokens) after fetch
- Pending tasks + static instructions: ~175 tokens

**Key insight:** Memory context is fetched at 3,000 token budget but then truncated to 1,000 chars for heartbeat. This means ghost_context does expensive work that gets thrown away. The 14 skills inject their full SKILL.md bodies into every system prompt.

**Dedup gaps:**
- No active deduplication during system prompt assembly
- Consolidation is async (happens after exchanges, not during)
- Ghost `compaction_suggested` flag triggers background reflect, but doesn't block loading dupes

---

### Finding 2: Multi-Agent Architecture (Fully Implemented)

Commit `6bc781a` delivered a complete multi-agent system:

**Config:** AgentIdentity struct with Name, BotUsername, BroadcastProbability, PeerBots, SystemPrompt

**Isolation:** Each agent gets its own:
- config.json, PID file, SQLite store, memory.db, bridge.sock, mcp.json
- Ghost namespace (e.g., agent:pikamini, agent:umbreonmini)
- Per-agent skills directory (~/.shell/agents/<name>/skills/)

**Group chat routing (shouldHandleGroupMessage):**
1. Reply-to this bot's message -> handle
2. @mention this bot -> handle (strip mention)
3. @mention peer bot -> skip
4. Bot-to-bot: 3 exchange limit, 30s cooldown, 15% probability
5. Human no-mention: roll against BroadcastProbability

**Onboarding:** New agents without identity memories get guided through self-discovery, storing pinned identity memories in ghost.

**CLI:** `shell multi start|stop|status|restart` manages all agents as separate daemon processes.

**Status:** Fully implemented but umbreonmini's first session was minimal (identity not configured, heartbeats all [noop]).

---

### Finding 3: Skill Inventory & Gaps

**14 skills total across 2 directories:**

| Skill | Type | Description |
|-------|------|-------------|
| hello | Global | Test greeting |
| weather | Global | City weather lookup |
| summarize | Global | File/URL summarization |
| web-search | Global | Brave/Tavily web search |
| generate-image | Global | Gemini image generation |
| browser | Global | Headless Chrome automation |
| shell-pm | Global | Background process manager |
| shell-tunnel | Global | Cloudflare tunnel exposure |
| shell-relay | Global | Cross-chat message relay |
| shell-schedule | Global | Cron/one-shot scheduling |
| shell-remember | Global | Ghost memory storage |
| shell-task | Global | Heartbeat task completion |
| lint-pr | Agent | PR linting |
| run-tests | Agent | Test runner |

**Skill authoring is dead simple:** SKILL.md with YAML frontmatter + optional scripts/ directory. Hot-loadable via `/skills reload`.

**Gaps identified:**
- No calendar/events skill (gog CLI exists but no skill wrapper)
- No health tracking skill (mami's illness tracked in scattered ghost memories)
- No usage/cost reporting skill (data exists in usage table but no skill)
- No ghost maintenance skill (consolidation/cleanup is manual)
- No notification/alert skill (failures are logged but not surfaced)
- TOOLS.md lists many CLI tools (gog, media converters) that aren't wrapped as skills

---

### Finding 4: Error Handling & Relay System

**Relay architecture (post-fixes):**
- MCP tool `shell_relay` is the current path (deprecated [relay] directives)
- Photo relay: read file -> send via Telegram API -> log to store (no Claude turn)
- Text relay: send directly + log to store (no longer blocks target session)
- 60s RPC timeout for all MCP calls

**Error handling patterns:**
- MarkdownV2 -> plain text fallback (good)
- Persistent process -> spawn-per-message fallback (good)
- Resume session -> fresh session fallback (good)
- Sticker download -> thumbnail -> text-only fallback chain (good)

**Gaps:**
- **No retry logic** on relay failures (fire-and-forget)
- **Silent failures** in SendPhoto (error logged, not surfaced to Claude)
- **No metrics/alerting** — 235+ slog calls but no Prometheus, no StatsD
- **No circuit breaker** for Telegram API
- **No relay delivery confirmation** back to sending user
- **context.Background() on Telegram sends** (no timeout)

---

### Finding 5: Store & Scheduler

**7 SQLite tables:** sessions, messages, message_map, schedules, usage, tasks

**Usage tracking exists!** The `usage` table tracks input/output tokens, cache tokens, cost_usd, and num_turns per exchange. `/usage` command surfaces this.

**Scheduler:** 1-minute tick loop, supports cron/once/heartbeat. Quiet hours (22:00-07:00). Noop suppression for heartbeats with nothing to report.

**Critical gap: No data cleanup.**
- Messages grow indefinitely (no retention policy)
- Completed tasks never deleted
- Disabled one-shot schedules accumulate
- No scheduled maintenance jobs
- StaleSessionChatIDs() exists but isn't called automatically

---

## Round 2: Usage Pattern Analysis

### Research Questions
1. How token-efficient are heartbeats vs interactive messages? (cost analysis)
2. What percentage of heartbeats are [noop] vs actionable?
3. Which skills are actually called most frequently?
4. What's the ghost memory growth rate and duplicate ratio?
5. What would a "daily digest" skill need from the existing store?
6. How could ghost memory hygiene be automated without a new skill?

### Finding 6: Heartbeat Noop Pattern

**Noop detection:** 12 hardcoded phrases + empty check, gated by <200 chars to prevent false positives.

**No persistent tracking.** Noop suppression is logged at INFO level but not persisted. The `usage` table has no field to distinguish heartbeat vs interactive exchanges. Only way to differentiate: string-match `[Heartbeat] ` prefix in messages table.

**Check-in cadence:** Every 4th heartbeat gets a "friendly check-in" hint appended. Counter is in-memory only, resets on daemon restart.

**Implication:** We can't answer "what % of heartbeats are noop" from existing data. Would need either: (a) add `source` column to usage table, or (b) add schedule_executions table.

---

### Finding 7: Ghost Memory Lifecycle

**Three-tier system:** sensory (raw, TTL decay) -> stm (working memory) -> ltm (long-term)

**Automated maintenance exists but is event-driven only:**
- `RunReflect()` called during every heartbeat (promotes/decays/deletes)
- `SummarizeExchanges()` called during heartbeat when exchanges > 10 (consolidates oldest)
- `CompactionSuggested` flag triggers background reflect during context fetch
- Heartbeat learnings auto-pruned to 20 per chat

**No scheduled maintenance.** Everything depends on heartbeats running. If heartbeats stop, no cleanup happens.

**Consolidation:** Creates summary nodes with "contains" edges. Children suppressed in context assembly but preserved for drill-down via ghost_expand. This is working well for exchange history.

**Key finding:** Memory hygiene is already mostly handled by heartbeat-triggered reflect cycles. The real problem from the ghost memory analysis was **duplicate semantic memories** (same fact stored multiple times), not exchange bloat. Reflect handles tier transitions but may not aggressively merge near-duplicates.

---

### Finding 8: Skill Token Waste (~1,500 tokens/message)

**All 14 skill SKILL.md bodies are injected into EVERY message's system prompt.** No filtering, no conditional loading.

**Total cost: ~1,400-1,600 tokens per message** (~1,115 words across 12 active skills plus formatting overhead).

**At 100 messages/day:** ~150K tokens/day wasted on skill instructions that are rarely all relevant to a single message.

**Biggest offenders:**
- browser: 233 words (rarely used)
- shell-schedule: 144 words
- generate-image: 126 words
- shell-pm: 113 words

**No optimization mechanisms exist.** The registry's `SystemPrompt()` has no parameters to filter skills.

**Opportunity:** Skill summaries (one-liner per skill, full body on-demand) could reduce to ~200-300 tokens, saving ~1,200 tokens/message.

---

### Finding 9: Daily Digest — Fully Feasible from Existing Data

**Available now:**
- Token costs per chat and global (GetUsageSummary, GetUsageAllChats)
- Session counts (sessions table with created_at)
- Message counts (messages table, filterable by role and created_at)

**Partially available:**
- Heartbeat count: infer from schedules.last_run_at, but no execution history
- Noop rate: not tracked

**Needed additions (small):**
- `GetMessageCount(chatID, since, role)` helper in store.go
- `GetSessionCount(chatID, since)` helper
- Optional: `schedule_executions` table for precise fire counts

**Format:** Could be a `/digest` command or a scheduled nightly summary.

---

## Round 3: Reflection & Prioritization

### Key Insights from Research

1. **Skill injection is the biggest token waste.** ~1,500 tokens/message for instructions rarely used in full. This is low-hanging fruit — a skill summary system or per-agent whitelist would save the most tokens per dollar.

2. **Ghost memory maintenance is surprisingly good.** Heartbeat-driven reflect + consolidation handles most lifecycle. The real gap is near-duplicate semantic memories (same fact stored slightly differently), which reflect doesn't aggressively merge.

3. **No observability into heartbeat efficiency.** Can't measure noop rate, can't differentiate heartbeat cost from interactive cost. Adding a `source` field to usage logging would unlock this.

4. **Daily digest is trivial to build.** All the data exists in SQLite. Just needs query helpers + formatting.

5. **Multi-agent is fully shipped but underutilized.** Umbreonmini exists but has no identity configured. The infrastructure is complete — the gap is content/personality, not code.

6. **No data cleanup anywhere.** Messages, tasks, and disabled schedules accumulate forever. This is a ticking time bomb for SQLite performance.

---

## Final Prioritized Feature List

### Tier 1: Token Efficiency (highest ROI)

| # | Feature | Effort | Tokens Saved | Description |
|---|---------|--------|-------------|-------------|
| 1 | **Skill summaries** | Low | ~1,200/msg | Split SKILL.md into one-liner summary + full body. Default to summary in system prompt. Load full body when skill is invoked. |
| 2 | **Heartbeat memory budget** | Trivial | ~750/heartbeat | Fetch memory at 1,000 char budget instead of 3,000 tokens then truncating. Avoid wasted ghost_context work. |
| 3 | **Usage source tagging** | Low | N/A (observability) | Add `source` field to usage table: "interactive", "heartbeat", "schedule". Enables cost analysis. |

### Tier 2: Operational Health

| # | Feature | Effort | Impact | Description |
|---|---------|--------|--------|-------------|
| 4 | **DB cleanup cron** | Low | Prevents SQLite bloat | Scheduled job to prune: messages older than 30d, completed tasks older than 7d, disabled one-shot schedules |
| 5 | **/digest command** | Low | Cost visibility | Daily summary: tokens, cost, message count, session rotations. Data already in store. |
| 6 | **Ghost near-dupe detection** | Medium | Token savings | During reflect cycle, detect and merge memories with >90% content similarity. Ghost may already support this via curate. |

### Tier 3: New Capabilities

| # | Feature | Effort | Impact | Description |
|---|---------|--------|--------|-------------|
| 7 | **Calendar skill** | Medium | Heartbeat integration | Wrap gog calendar commands. Heartbeat can check "upcoming events in next 2 hours" during enrichment. |
| 8 | **Umbreonmini bootstrap** | Low | Multi-agent usage | Configure identity, personality, and unique skills for the second agent. Code is done — just needs content. |
| 9 | **Voice message skill** | Medium | Family delight | TTS via edge-tts or similar. Send voice notes to Telegram for care messages to mami. |
| 10 | **Health log skill** | Low-Med | Structured tracking | SQLite table for symptoms/temp/meds. Replace scattered ghost memories with queryable data. |

### Tier 4: Future Considerations

| # | Feature | Effort | Notes |
|---|---------|--------|-------|
| 11 | Schedule execution history | Low | Add table for precise heartbeat/schedule analytics |
| 12 | Relay delivery confirmation | Medium | Return success/failure to sending user |
| 13 | Metrics endpoint | Medium | Lightweight stats for monitoring |

---

## Round 4: Agent Self-Evolving Skills (OpenClaw Moment)

### Research Questions
1. What does the current skill authoring flow look like end-to-end?
2. What patterns do other frameworks (OpenClaw, Voyager, LATM, BabyAGI) use for self-extending agents?
3. Where does the shell learning loop break down for skill synthesis?
4. How could agents share skills with each other?

---

### Finding 10: Current Skill Authoring Flow (Almost There)

The foundation exists but has one critical gap:

**What works today:**
1. Skill-authoring instructions seeded as pinned procedural memory at startup
2. Agent knows its per-agent skills dir: `~/.shell/agents/<name>/skills/`
3. Agent has full filesystem access (bypassPermissions mode)
4. Agent can write SKILL.md + scripts/ via Bash
5. `/skills reload` hot-loads new skills into the registry
6. New skills appear in system prompt on next message

**The single blocker:** No RPC endpoint for `/skills-reload`. Agent must ask the user to run `/skills reload` manually. The heartbeat learning -> skill creation loop is broken at the last step.

**Fix:** Add `POST /skills-reload` to internal/rpc/server.go with a callback to bridge.ReloadSkills().

---

### Finding 11: Industry Patterns for Self-Extending Agents

| Pattern | Source | How It Works | Shell Applicability |
|---------|--------|-------------|-------------------|
| **LATM (Tool Makers)** | ICLR 2024 | Strong model writes tool once, weak model uses it | Opus forges skills, cheaper model uses them in heartbeats |
| **Voyager Skill Library** | NVIDIA/MineDojo | Write code -> execute -> verify -> store if works | Agent writes script -> tests via Bash -> stores as skill if passes |
| **BabyAGI functionz** | Yohei Nakajima | Self-building agent stores/manages functions in DB | SQLite-backed skill registry with usage tracking |
| **OpenClaw Self-Authored** | OpenClaw | Agent writes SKILL.md from observed patterns | Identical to shell's skill format, just needs the autonomous loop |
| **Progressive Disclosure** | Claude Agent SDK | Catalog injected, full body loaded on-demand | Skill summaries in prompt, full body via tool call |
| **Reflexion** | ALFWorld | Fail -> critique -> retry -> store successful trajectories | Heartbeat learnings already capture this partially |

---

### Finding 12: The Skill Evolution Loop (What's Missing)

**Current loop (memory-only):**
```
User conversation → Agent learns pattern → shell-remember stores insight
→ Heartbeat recalls insights → Agent applies knowledge → Better responses
```

**Desired loop (skill synthesis):**
```
User conversation → Agent notices repeated workflow
→ Agent writes SKILL.md + test script → Agent tests script via Bash
→ Agent verifies output → Agent triggers /skills-reload via RPC
→ Skill loaded into system prompt → Agent uses skill autonomously
→ Heartbeat evaluates skill effectiveness → Prune unused skills
```

**Gaps to close:**

| Gap | Effort | Description |
|-----|--------|-------------|
| `/skills-reload` RPC endpoint | Low | Wire handleSkillsReload in rpc/server.go → bridge.ReloadSkills() |
| Skill test-before-store | Low | Agent convention: run script, verify output before committing SKILL.md |
| Skill usage tracking | Medium | Log which skills are invoked, how often, success rate |
| Skill pruning | Low | Heartbeat checks: skills unused for 14d flagged for removal |
| Cross-agent skill sharing | Medium | Shared skill directory or "publish" command from per-agent to global |
| Skill approval gate | Low | Config flag: require user emoji reaction before persisting new skill |

---

### Finding 13: Cross-Agent Skill Sharing

**Current state:** Each agent's skills are isolated in `~/.shell/agents/<name>/skills/`. No mechanism for sharing.

**Three-tier sharing model (proposed):**
```
~/.shell/skills/              ← Global (all agents see these)
~/.shell/agents/<name>/skills/ ← Per-agent (only this agent)
~/.shell/shared-skills/        ← NEW: Shared pool (agents publish here)
```

**Sharing flow:**
1. Agent creates skill in per-agent dir, tests it, uses it
2. After N successful uses, agent proposes promoting to shared
3. User approves (or auto-approve if confidence threshold met)
4. Skill copied to shared dir, available to all agents on next reload

**Alternative: Git-based sharing**
- Agent commits skill to `.agent/skills/` in the repo
- Other agents pull changes on reload
- Already works if both agents share the same workdir

---

### Finding 14: Progressive Disclosure (Token Optimization + Scale)

As agents create more skills, the "dump all skills into prompt" approach breaks down. OpenClaw and Claude Agent SDK both solve this with progressive disclosure:

**Current:** All 14 skills → ~1,500 tokens in every system prompt
**With self-authored skills:** Could grow to 30-50 skills → 3,000-5,000 tokens wasted

**Proposed architecture:**
```
System prompt contains:
  ## Available Skills (catalog only)
  - browser: Automate headless Chrome
  - web-search: Search the web
  - my-new-skill: Does something useful
  ... (one line each, ~20 tokens total for 20 skills)

When agent needs a skill:
  → Calls internal tool: load_skill(name="browser")
  → Full SKILL.md body injected into conversation context
  → Agent uses the skill with full instructions
```

**Implementation:** Convert `skillsSystemPrompt()` to emit catalog-only. Add a `load_skill` MCP tool or conversation-injected tool that returns the full skill body on demand.

---

## Revised Prioritized Feature List (Including Skill Evolution)

### Tier 0: Enable Self-Evolution (the OpenClaw moment)

| # | Feature | Effort | Impact | Description |
|---|---------|--------|--------|-------------|
| 0a | **`/skills-reload` RPC endpoint** | Low | Critical enabler | Wire rpc/server.go → bridge.ReloadSkills(). Removes the only blocker to autonomous skill creation. |
| 0b | **Skill test convention** | Trivial | Quality gate | Update skill-authoring seeded memory to include: "Always test your script via Bash before writing SKILL.md. Verify it produces expected output." |
| 0c | **Skill catalog system prompt** | Medium | Token savings + scale | Replace full skill body injection with one-liner catalog. Add `load_skill` tool for on-demand body retrieval. Saves ~1,200 tokens/msg AND scales to unlimited skills. |

### Tier 1: Token Efficiency

| # | Feature | Effort | Tokens Saved | Description |
|---|---------|--------|-------------|-------------|
| 1 | **Heartbeat memory budget** | Trivial | ~750/heartbeat | Fetch memory at 1,000 char budget instead of 3,000 tokens then truncating |
| 2 | **Usage source tagging** | Low | N/A (observability) | Add `source` field to usage table for cost analysis |

### Tier 2: Operational Health

| # | Feature | Effort | Impact | Description |
|---|---------|--------|--------|-------------|
| 3 | **DB cleanup cron** | Low | Prevents SQLite bloat | Prune old messages, completed tasks, disabled schedules |
| 4 | **/digest command** | Low | Cost visibility | Daily summary using existing store data |
| 5 | **Skill usage tracking** | Medium | Skill lifecycle | Log invocations to SQLite. Enable pruning of unused skills. |
| 6 | **Skill pruning in heartbeat** | Low | Prevents skill bloat | Heartbeat flags skills unused for 14d |

### Tier 3: Multi-Agent Growth

| # | Feature | Effort | Impact | Description |
|---|---------|--------|--------|-------------|
| 7 | **Cross-agent skill sharing** | Medium | Ecosystem growth | Shared skill directory or publish-to-global mechanism |
| 8 | **Umbreonmini bootstrap** | Low | Second agent operational | Identity, personality, unique skills |
| 9 | **Calendar skill** | Medium | Daily life integration | First "self-useful" skill that agents would want to create anyway |

### Tier 4: Advanced Evolution

| # | Feature | Effort | Impact | Description |
|---|---------|--------|--------|-------------|
| 10 | **Skill approval gate** | Low | Safety | Config flag: user reaction required before persisting new skill |
| 11 | **Voyager-style self-verification** | Medium | Quality | Agent tests skill, checks output, iterates before storing |
| 12 | **Skill composition** | High | Complex workflows | Agent combines simple skills into compound skills |
| 13 | **LATM cost optimization** | Medium | Efficiency | Opus forges skill once, cached execution for future use |

---

## Round 5: Multi-Agent Collaboration (2026-03-28)

### Context

Pikamini and umbreonmini run as isolated agents in a shared Telegram group chat. Both are in "autonomous" mode (broadcast=1.0), both receive all messages. A shared transcript + task delegation system was built but immediately decoupled — each agent now has its own transcript DB. The task delegation code exists (task_directives.go) but is broken because tasks are stored per-agent, not shared.

### Research Questions

1. How do existing frameworks (AutoGen, CrewAI) handle agent-to-agent task delegation?
2. What are the codebase constraints on cross-agent triggering?
3. What collaboration patterns make sense for a family companion chat vs enterprise?
4. Should tasks be synchronous or asynchronous?
5. What's the minimum viable experiment to test if collaboration adds value?

---

### Finding 15: AutoGen GroupChat Speaker Selection

AutoGen's `SelectorGroupChat` uses an **LLM-based speaker selector**. After each message, a model evaluates conversation history + agent descriptions to pick who speaks next.

**Selection methods:**
- `auto` (default): LLM prompt selects next speaker
- `round_robin`: Fixed circular order
- `random`: Random selection
- `selector_func`: Custom callable returning agent name or None (falls back to LLM)
- `candidate_func`: Filters eligible agents before LLM selects

**Task delegation:** No explicit delegation primitive. The orchestrator routes turns. Agents implicitly delegate by saying "Agent X should handle this" and the selector picks up on it.

**Key design:** `allow_repeated_speaker=false` prevents one agent monopolizing.

**Applicability to shell:** Our autonomous mode is closest to `auto` — each agent independently decides. We lack the orchestrator. Adding one would mean a third process or an LLM call per message, which is expensive for a family chat.

---

### Finding 16: CrewAI Delegation = Internal Tool

CrewAI implements delegation as a **tool injected into the agent's toolset** when `allow_delegation=True`.

**Flow:**
1. Agent with delegation enabled gets `DelegateWorkTool`
2. Agent calls the tool specifying task + target agent
3. Delegate executes **synchronously** within the tool call
4. Result returned as tool output to the original agent
5. Original agent incorporates result into its response

**Key details:**
- Delegation is disabled by default (was causing issues)
- New `allowed_agents` parameter restricts who you can delegate to
- Hierarchical mode: auto-created manager agent coordinates everything

**Applicability:** The "delegation as a tool" pattern maps directly to `shell-task` skill. The difference: CrewAI delegates synchronously (same turn), our Telegram setup is inherently async (separate processes).

---

### Finding 17: SQLite-Based Agent Mail Systems

**Overstory** (github.com/jayminwest/overstory):
- SQLite mail system in WAL mode
- 8 typed message types: `worker_done`, `merge_ready`, `dispatch`, `escalation`
- Broadcast addressing: `@all`, `@builders`, `@scouts`
- Agents query inbox via CLI, messages injected into session context
- Agents run in isolated git worktrees

**MCP Agent Mail** (github.com/Dicklesworthstone/mcp_agent_mail):
- Agents get memorable identities, inboxes, outboxes
- Dual persistence: Git (human-readable) + SQLite with FTS5
- File reservation "leases" prevent edit conflicts
- HTTP-only protocol, bearer token auth, exposed as MCP tools

**Applicability:** The Overstory pattern is very close to what we're building. The key insight: messages are **injected into session context** — the agent sees pending mail as part of its input, same as our pending task injection.

---

### Finding 18: Codebase Constraints on Cross-Agent Triggering

**Direct @mentions bypass all bot-to-bot limits.** In `shouldHandleGroupMessage()`, the `mentionsMe` check (line 1028-1030) returns true immediately, before any exchange limit or cooldown checks. A task trigger @mention will always be delivered.

**Transport.Notify can send @mentions.** `Notify(chatID, msg)` sends plain text, which CAN include `@bot_username`. The handler's `parseMentions()` regex matches `@(\w+)` in message text regardless of whether it's a Telegram entity or plain text.

**Per-chat locking queues, doesn't drop.** If the target agent is busy, the trigger message gets a clock reaction and waits in a mutex queue. No messages are lost.

**Bot username NOT in skill env.** `SHELL_CHAT_ID` and `SHELL_BRIDGE_SOCK` are available, but the skill script doesn't know which agent it belongs to. Need to add `SHELL_AGENT_NAME` or `SHELL_BOT_USERNAME` to process/manager.go.

**Each agent has its own bridge.sock.** The RPC server runs per-agent. For a shared task store, both RPC servers need to open the same SQLite DB (WAL mode handles concurrent access).

---

### Finding 19: Failure Handling Patterns

**Timeouts:** Google ADK recommends user-facing 10-15s, primary agents 30s, fallback 60s.

**Key failure mode:** Independent (non-communicating) agents amplify errors 17.2x vs single agent. Centralized architectures contain this to 4.4x.

**Engineering rule:** Treat agents as distributed systems. Validate at boundaries. Checkpoint state at transitions.

**For shell-task:** Tasks should have a TTL (e.g., 10 minutes). Expired tasks auto-fail with notification to originator. This prevents zombie tasks when an agent crashes or gets stuck.

---

### Finding 20: When Multi-Agent Actually Helps (Benchmarks)

**LangChain tau-bench results:**
- Single agent accuracy drops sharply with 6+ distractor tools
- Swarm (agents respond directly) outperformed Supervisor (only orchestrator responds)
- After optimization, supervisor achieved ~50% performance increase

**Finance Agent benchmark:** +80.9% improvement with task decomposition

**When it helps:** Decomposable tasks, 10+ tools, breadth-first research, code+test workflows, factual verification

**When it hurts:** Tightly-coupled sequential reasoning, creative tasks, 2-6x token cost increase, tasks where single agent achieves >45% success

**Bottom line:** Always benchmark single-agent first. Multi-agent wins on decomposable/parallel tasks.

---

### Finding 21: Companion Bot Multi-Agent (No Precedent)

**No found examples** of companion bots doing maker-checker or task delegation in family chats. Closest:
- **Memoh**: Multiple bots with persistent memory, cross-channel inbox
- **Multi-Bot Telegram System**: Mention-based isolation only, no collaboration
- Most multi-bot setups are enterprise or development-focused

**Implication:** We're in uncharted territory. No proven patterns exist for family companion multi-agent. We need to experiment and see what feels natural.

---

## Open Questions for Discussion

### Q1: What collaboration patterns fit a family group chat?
Enterprise patterns (maker-checker, orchestrator-worker) feel corporate. In a family chat:
- Is "one researches, one verifies" natural or forced?
- Should collaboration be invisible (agents naturally build on each other) or explicit (visible task delegation with Telegram messages)?
- Would the family find it charming or annoying to see bots talking to each other?

### Q2: Synchronous vs asynchronous tasks?
- **Synchronous** (CrewAI style): Agent A delegates within the same turn, waits for result. Simpler but impossible with separate Telegram bots.
- **Asynchronous** (Overstory style): Agent A delegates, continues. Agent B picks it up later, posts result. More natural for chat.
- Our architecture forces async (separate OS processes). The question is: how does the result get surfaced? Telegram message? Injected into next context?

### Q3: Should task triggers be visible?
- **Visible:** Telegram message `"@umbreon_mini_bot 📋 Task from pikamini: verify this"` — family sees collaboration happening
- **Silent:** Shared DB poll, no Telegram message — less noise but invisible to humans
- **Hybrid:** Silent for internal tasks, visible only when it produces a result for the human

### Q4: What should a task actually contain?
- Just the description? Or also conversation context?
- If pikamini says "verify the health advice I just gave mami" — does the task include what pikamini said? Or does umbreon need to figure it out from the transcript?
- With separate transcripts, umbreon can't see what pikamini said unless it's in the task description.

### Q5: Should we start with prompt-only experiment first?
Before building shell-task, we could test with just prompt changes:
- Add to groupAgentPrompt: "For health/medical advice, @mention the other agent to independently verify"
- See if agents naturally do it via plain Telegram @mentions
- No new code, just prompt engineering
- If it works well, then formalize with shell-task

### Q6: How to handle task results?
When umbreon completes a task:
- Post result as a regular Telegram message? (pikamini sees it in transcript, human sees it too)
- Post result only to pikamini via task store? (invisible to humans)
- Both? (visible message + structured result in task store)

### Q7: What tasks would actually benefit from two agents?
Concrete examples for this family:
- Health advice verification (pikamini researches, umbreon cross-checks)
- Recipe suggestions (one finds recipe, other checks dietary constraints)
- Event planning (one handles logistics, other handles emotional/social aspects)
- Code review (if either agent is asked to write code)
- Translation verification (one translates, other checks accuracy)
- Are these real use cases or are we forcing it?

### Q8: Should agents develop their own collaboration patterns?
Instead of prescribing when to delegate:
- Give them the shell-task tool
- Add minimal prompting about peer capabilities
- Let them figure out when to use it organically
- Monitor what emerges and refine based on actual usage
- This is the "let the agents be agents" approach

### Q9: Task TTL and failure handling?
- What happens if a task is never completed? TTL of 10 minutes? 1 hour?
- Should expired tasks auto-fail with notification?
- Should the originator retry or handle it themselves?
- How to prevent a task from triggering an infinite delegation loop (A→B→A→B...)?

---

## Answers (2026-03-28)

### A1: Natural collaboration, not forced
Agents should naturally collaborate — it should feel fun, not corporate. No prescriptive rules like "always verify health advice." Let them figure out organically when to involve each other.

### A2: Async, push for concurrency
Async is the right model. Push for concurrency — this is why we're in Go with goroutines. Tasks should not block the originating agent.

### A3: Visible but not verbose
Task triggers and results should be visible in the group chat so humans see collaboration happening. But keep it concise — don't spam walls of text.

### A4: High-level description + invisible metadata
Task description should be human-readable and high-level. But include invisible metadata (goal_id, context summary, originating chat_id) to help the receiving agent understand context without sharing raw conversation.

### A5: shell-task for single-agent too
shell-task isn't just for multi-agent — a single agent should use it to decompose complex work into subtasks. This means shell-task is a general task management skill, not just a delegation mechanism. An agent can create tasks for itself.

### A6: Both visible message + task store
Results posted as Telegram messages (everyone sees) AND stored in task DB (structured, queryable for reflection).

### A7: Prompt agents to consider task decomposition
Update the prompt so agents consider: "should I break this into tasks first? Should I pull in another agent?" before diving in. Essentially encourage planning-first thinking.

### A8: Let agents develop their own patterns, reflect weekly
Give agents the tool + minimal guidance. Let them figure out collaboration organically. Capture enough data (task creation, completion, results, timing) to enable weekly reflection — either manually in claude or agents do it automatically during heartbeat. The goal is a learning loop.

### A9: Tasks linked to original goal, TTL okay
Every task should be associated with the original goal/request so we can trace the full chain. TTL is fine for cleanup but the task + result should be preserved for reflection even after TTL. We want to learn from completed task chains, not just discard them.

---

## Design Implications from Answers

### shell-task is a universal task skill, not just delegation
- Single agent uses it to decompose work: `shell-task create --to self --description "step 1: research"`
- Multi-agent uses it for delegation: `shell-task create --to umbreon_mini_bot --description "verify this"`
- Same skill, same DB, same reflection data

### Task schema needs goal tracking
```
tasks:
  id, chat_id, goal_id (nullable, links related tasks),
  from_agent, to_agent (can be same agent),
  description, metadata (JSON: context summary, etc.),
  status, result,
  created_at, updated_at, ttl
```

### Reflection data requirements
To learn weekly, we need:
- Task creation rate per agent
- Completion rate, avg time-to-complete
- Self-tasks vs delegation tasks ratio
- Which delegation patterns emerge (who delegates to whom, for what)
- Task chains (goal_id grouping)
- Success quality (were results used? did human react positively?)

### Prompt evolution
groupAgentPrompt should encourage:
1. "Consider if this request would benefit from breaking into subtasks"
2. "You can create tasks for yourself or delegate to peer agents"
3. "Peer agents and their strengths: [list]"
4. NOT prescribe specific scenarios — let agents discover patterns

---

## Round 6: Deep Dive — Existing Infrastructure & Patterns (2026-03-28)

### Finding 22: Two Task Systems Already Exist (Don't Build a Third)

The codebase already has TWO task systems:

**System A: Background Tasks** (`store.tasks` table)
- Simple queue: `/task add "description"` → heartbeat picks it up → `shell-task complete --id X`
- Per-chat, per-agent. No delegation, no goal tracking.
- User-facing commands: `/task add|list|done|delete`
- RPC endpoint: `POST /task` (only action="complete")

**System B: A2A Delegated Tasks** (`transcript.tasks` table)
- Agent-to-agent: `[task to=umbreonmini]...[/task]` → parsed by bridge → stored in transcript DB
- Has from_agent, to_agent, status lifecycle, result field
- Currently broken (separate transcript DBs)

**Design decision: Unify into one system.** shell-task should replace BOTH:
- Self-tasks (System A use case): `shell-task create --to self --description "..."`
- Delegation (System B use case): `shell-task create --to umbreon_mini_bot --description "..."`
- Same DB, same schema, same reflection data

### Finding 23: The Planner Is Separate (And Should Stay Separate)

The existing planner (`internal/planner/`) is a code-specific execute→test→review loop:
- Parses markdown checklists into sequential tasks
- Runs in git worktrees with test verification
- Has its own state machine (drafting→executing→blocked→done)
- Auto-retries with reviewer feedback

**shell-task is NOT the planner.** The planner is for structured code execution with git and tests. shell-task is for general task decomposition and agent collaboration. They're complementary:
- Complex code change → use planner (`/plan`)
- Break a request into steps or delegate → use shell-task
- Planner COULD create shell-tasks for tracking, but that's a future integration

### Finding 24: Self-Task Decomposition Patterns

**Plan-and-Execute** (LangGraph): LLM generates a list of steps → state machine iterates → optional replan after each step. No explicit "subtask objects" — just flat lists with index tracking.

**Reflexion**: Iterative self-correction — execute, critique, revise. Structured critique via `{missing, superfluous}` fields.

**Key insight for shell-task:** An agent doesn't need a complex state machine. It just needs:
1. Break request into subtasks: `shell-task create` for each
2. Work through them sequentially (or delegate some)
3. Mark each done with result
4. All linked by goal_id for traceability

### Finding 25: Goal-Task Hierarchy Design

Research shows a simple two-level hierarchy works well:

```
Goal (the original user request)
├── Task 1 (self or delegated)
├── Task 2 (self or delegated, may depend on 1)
├── Task 3 (self or delegated)
└── Final synthesis task
```

**Properties for good decomposition:**
- **Solvability**: Each subtask is achievable independently
- **Completeness**: All subtasks together achieve the goal
- **Non-redundancy**: No overlapping work

**35-minute degradation**: Research shows agents degrade after ~35 min of continuous work. Subtasks should be sized to fit within fresh context windows. This is natural for Telegram — each message is a fresh context window anyway.

### Finding 26: What Reflection Data to Capture

From ERL (Experiential Reflective Learning) and practical frameworks:

```
Per task:
- description, from_agent, to_agent
- goal_id (links to parent goal)
- status, result
- created_at, completed_at (→ time-to-complete)
- token_cost (if measurable)
- outcome_signal: success/failure/partial

Per goal:
- original_request (what the human asked)
- chat_id
- total_tasks, completed_tasks, failed_tasks
- total_time
- human_satisfaction (did they react positively? follow up with complaints?)
```

**Weekly reflection prompt:**
```
Here are the tasks from the last 7 days:
- X goals decomposed into Y total tasks
- Z delegated to peer agents, W self-tasks
- Average completion time: N minutes
- Failed tasks: [list]
- Patterns: [auto-generated]

What worked well? What should change?
```

### Finding 27: No Precedent for Companion Bot Retrospectives

No framework implements explicit "weekly retrospectives." Closest patterns:
- **ERL**: Periodic heuristic pruning based on relevance scores
- **EvolveR**: Experience-driven lifecycle (execute → reflect → distill → prune)
- **Multi-level Reflection**: Step-level, trajectory-level, and cross-task-level patterns

**For shell:** The heartbeat is the natural reflection trigger. Add a weekly cadence:
- Every heartbeat: check pending tasks, do maintenance
- Weekly (or every N heartbeats): aggregate task data, run reflection prompt, store learnings

### Finding 28: Task Notification UX for Telegram

**Best practice: In-place message editing.** Send initial status, then `editMessageText` to update as progress changes. Avoids clutter.

**For shell-task triggers:**
- Task created: `"📋 pikamini → @umbreon_mini_bot: verify health advice (task abc123)"` — one line, visible, not verbose
- Task completed: `"✅ umbreonmini completed task abc123"` — even shorter
- Task failed: `"❌ task abc123 failed: [brief reason]"`

**For self-tasks (agent working through its own decomposition):**
- Don't spam each subtask to Telegram
- Only show the initial decomposition and final result
- In-progress updates only if the human asked for them

---

## Revised Open Questions

### Q10: Should shell-task replace both existing task systems?
System A (background tasks) and System B (A2A delegation) overlap conceptually. Unifying them means:
- One table, one skill, one RPC endpoint
- But System A is user-facing (`/task add`), System B is agent-facing
- Do we merge the user commands too, or keep `/task` as a wrapper around shell-task?

### Q11: Where does the unified task DB live?
Options:
- Per-agent store DB (current System A location) — means delegation tasks are per-agent, broken for multi-agent
- Shared DB at `~/.shell/shared/tasks.db` — works for delegation, but single-agent self-tasks don't need sharing
- Both: self-tasks in per-agent DB, delegation tasks in shared DB — adds complexity
- **Simplest: always use shared DB.** Even self-tasks go there. Single-agent still works (it's just reading its own tasks). Multi-agent works because both can see delegated tasks.

### Q12: What's the shell-task create flow for delegation?
When pikamini runs `shell-task create --to umbreon_mini_bot --description "verify this"`:
1. Script calls RPC → creates task in shared DB
2. RPC handler sends Telegram @mention to trigger umbreon
3. Umbreon's handler processes the @mention, sees pending task in context
4. Umbreon works on it, runs `shell-task complete --id X --result "..."`
5. RPC handler updates task, sends Telegram notification to pikamini

**But:** Each agent has its own RPC server on its own socket. Pikamini's RPC can create the task and send the Telegram trigger. Umbreon's RPC completes the task and sends the completion trigger. Both read/write the same shared tasks.db. This should work.

### Q13: How much metadata is "invisible metadata"?
The user said tasks should have "invisible metadata to help." Options:
- `metadata JSON` column with: context summary, originating message snippet, relevant memory keys
- Keep it minimal at first — just goal_id and maybe a one-line context summary
- Let it grow organically based on what agents actually need

---

## Round 7: Scheduler Integration & Internal Message Bus (2026-04-14)

### Finding 29: Scheduler Is the Agent's Autonomous Executor

The scheduler is the only component that can trigger agent actions WITHOUT a human message. It runs a 1-minute tick loop with three callback types:

| Callback | What it does | Returns response? |
|----------|-------------|-------------------|
| `NotifyFunc` | Sends text to Telegram (no Claude) | No |
| `PromptFunc` | Routes through Claude as if user sent it | No (fire-and-forget) |
| `HeartbeatPromptFunc` | Routes through Claude, captures response for noop detection | Yes |

**Key insight**: `PromptFunc` is the bridge between "something happened" and "Claude processes it." If a task arrives for this agent, we can use a schedule (or a schedule-like mechanism) to trigger a Claude session that processes the task.

### Finding 30: SystemChatID = 0 Is the Agent's Inner Monologue

`ChatID = 0` is a reserved phantom chat where heartbeats run. It aggregates context from ALL real chats. Telegram skips delivery for SystemChat. This is where agent-level reflection and task processing naturally belongs.

**For shell-task**: When a delegated task arrives, we could trigger a prompt in the originating chat's session (so the agent has the right conversation context) or in SystemChat (for agent-level tasks). The choice depends on whether the task needs chat-specific context.

### Finding 31: No Daemon-to-Daemon IPC Exists

Each agent has its own:
- `bridge.sock` (RPC server, Unix socket)
- Store DB, memory DB, transcript DB
- Scheduler, process manager

**There is no cross-agent communication channel today.** The only bridge is Telegram itself — one agent sends a message to the group, the other's handler picks it up.

### Finding 32: Internal Message Bus Options

The user prefers "internal message bus for communication + Telegram for status changes." Options:

**Option A: Shared Unix Socket**
- New socket at `~/.shell/shared/task-bus.sock`
- Lightweight HTTP server (like RPC) that both agents connect to
- Problem: Who runs this server? A third process? Adds complexity.

**Option B: Shared SQLite + File Watch**
- Tasks written to `~/.shell/shared/tasks.db`
- Each agent polls or uses `fsnotify` to watch for changes
- SQLite WAL mode handles concurrent access
- Polling interval: 5-10 seconds (lightweight)
- Pro: No new process, just a DB + poll loop
- Con: Not instant (5-10s delay)

**Option C: Named Pipes / Unix Signals**
- Each agent listens on a named pipe: `~/.shell/agents/<name>/task.pipe`
- Writer pushes task notification, reader triggers processing
- Pro: Instant delivery, zero polling
- Con: Fragile, OS-specific, needs careful cleanup

**Option D: Each Agent's RPC as a Target**
- Pikamini's RPC knows umbreon's socket path (from peer discovery)
- When creating a task, pikamini's RPC calls umbreon's `/task-notify` endpoint directly
- Pro: Uses existing infrastructure, instant, bidirectional
- Con: One agent's process reaching into another's socket — tight coupling

**Option E: Scheduler-Based Task Processing**
- Tasks written to shared DB
- Each agent's scheduler has a "task check" schedule (every 1-5 minutes)
- Scheduler fires `PromptFunc` when pending tasks exist for this agent
- Pro: Zero new infrastructure — just a schedule entry + shared DB
- Con: Up to 5 minutes delay, but heartbeat already runs hourly

### Finding 33: The Scheduler + Shared DB Pattern (Recommended)

Combining the scheduler with a shared task DB gives us an internal message bus with zero new infrastructure:

```
Agent A creates task:
  shell-task create --to umbreon --description "verify this"
    → writes to ~/.shell/shared/tasks.db
    → optionally sends Telegram status: "📋 task created for @umbreon"

Agent B's scheduler tick (every 1 min):
  → checks shared tasks.db for pending tasks addressed to this agent
  → if found: fires PromptFunc(chatID, "[Task] You have pending tasks: ...")
  → Claude processes the task, calls shell-task complete
  → optionally sends Telegram status: "✅ task completed"
```

**Why this works:**
- Scheduler already runs every minute — adding a task check is trivial
- `PromptFunc` already routes through Claude with full session context
- Shared DB handles concurrency via WAL mode
- No new daemons, sockets, or IPC mechanisms
- Telegram messages are optional status updates, not the trigger mechanism
- Tasks are processed within 1 minute (acceptable for async collaboration)

**The scheduler becomes the event bridge.** It already triggers heartbeats; now it also triggers task processing.

### Finding 34: Self-Scheduling via shell-task

An agent scheduling a task for itself is just:
1. `shell-task create --to self --description "step 2: analyze results"`
2. Task goes to shared DB with `to_agent = this_agent`
3. Next scheduler tick: agent sees its own pending task
4. Scheduler triggers `PromptFunc` → Claude processes it

This enables:
- **Task decomposition**: Agent breaks work into steps, schedules each
- **Deferred work**: "I'll do this later" → create a task for self
- **Heartbeat pickup**: Existing heartbeat already shows pending tasks — now it can also process self-tasks

### Finding 35: Task + Schedule Convergence

Tasks and schedules are converging:

| Feature | Schedule | Task |
|---------|----------|------|
| Fires at specific time | Yes (cron/once) | No (ASAP) |
| Fires on demand | No | Yes (next tick) |
| Has a target agent | Implicit (this agent) | Explicit (self or peer) |
| Has a result | No | Yes |
| Linked to a goal | No | Yes (goal_id) |
| Processed by Claude | Yes (PromptFunc) | Yes (via scheduler) |

**They share the same execution path** — the scheduler's `PromptFunc`. The difference is trigger timing (scheduled vs on-demand) and metadata (result, goal_id).

A future "scheduled task" would combine both: "Run this task at 3pm tomorrow" — a task with a schedule. But that's a later optimization.

---

## Answers Round 2 (2026-04-14)

### A10: Unify into one system
Yes — one task system for both self-tasks and delegation. The existing `/task add` commands become wrappers around shell-task.

### A11: Shared DB, configurable later
Start with `~/.shell/shared/tasks.db`. Add `task_store_path` config option for flexibility.

### A12: Internal message bus + Telegram for status
The scheduler-based approach: tasks in shared DB, each agent's scheduler checks for pending tasks every tick (1 min). Telegram only for human-visible status updates, not as the trigger mechanism. This is the internal message bus.

### A13: Minimal metadata, grow as needed
Start with goal_id + one-line context. Add fields when agents actually need them.

---

## Revised Open Questions

### Q14: Should the scheduler task check be its own schedule type?
Options:
- Add a new schedule type "task-poll" alongside cron/once/heartbeat
- Or just add task checking to the existing tick loop (simpler, but less configurable)
- Or make it a check within heartbeat processing (tasks already shown in heartbeat context)

### Q15: What chat context should delegated tasks run in?
When umbreon processes a task from pikamini:
- Run in the originating chat's session (has conversation context but may confuse the session)
- Run in SystemChat (agent-level, clean context, but no chat history)
- Run in a dedicated "task" chat context (new concept, fully isolated)

### Q16: How to prevent the scheduler from firing a task prompt while the agent is already busy?
The per-chat mutex handles this for the same chat. But if a task prompt fires for chat X while the agent is handling a message in chat X, it queues. If the task runs in SystemChat, it could conflict with heartbeat. Need to consider session concurrency.

### Q17: Should Telegram status messages be editable?
Instead of sending separate "created" and "completed" messages:
- Send one message on create: "📋 Task for @umbreon: verify this"
- Edit it on complete: "✅ Task completed: looks good"
- Reduces chat clutter, keeps status in one place

---

## Answers Round 3 (2026-04-14)

### A14: Extend the scheduler foundation
Task polling should be a new schedule type — extend the scheduler rather than bolt on a separate mechanism. This keeps task execution within the same well-tested tick loop, with the same quiet hours, concurrency handling, and callback patterns.

### A15: New task context
Delegated tasks run in their own fresh conversation context — not the originating chat's session and not SystemChat. Each task gets a clean slate. This prevents task processing from polluting ongoing conversations and keeps sessions focused.

### A16: Each task = new session, no concurrency issue
Since each task runs in a new context (fresh Claude session), there's no conflict with existing chat sessions. The agent can process a task in parallel with ongoing conversations. The scheduler fires a prompt in a task-specific context, Claude processes it, done.

### A17: Yes, editable Telegram status messages
Send one message on task create, edit it in-place on status changes. Reduces clutter, keeps task lifecycle in one visible message.

---

## Consolidated Design Understanding (2026-04-14)

### Architecture Summary

```
┌─────────────────────────────────────────────────────┐
│                ~/.shell/shared/tasks.db              │
│  (unified task store — self-tasks + delegation)      │
└──────────┬──────────────────────────┬────────────────┘
           │                          │
     ┌─────┴─────┐             ┌─────┴─────┐
     │ Pikamini  │             │ Umbreon   │
     │ Daemon    │             │ Daemon    │
     │           │             │           │
     │ Scheduler │             │ Scheduler │
     │  tick 1m  │             │  tick 1m  │
     │  ├ heartbeat             │  ├ heartbeat
     │  ├ cron/once             │  ├ cron/once
     │  └ task-poll ◄──────────┼──┘ task-poll
     │       │                 │       │
     │  PromptFunc             │  PromptFunc
     │  (new task ctx)         │  (new task ctx)
     │       │                 │       │
     │  Claude CLI             │  Claude CLI
     │  (fresh session)        │  (fresh session)
     │       │                 │       │
     │  shell-task complete    │  shell-task complete
     │       │                 │       │
     └───────┼─────────────────┼───────┘
             │                 │
        Telegram Group Chat
        (editable status messages only)
```

### Key Design Decisions

1. **One task system** — unifies background tasks (System A) and delegation (System B)
2. **Shared SQLite DB** — `~/.shell/shared/tasks.db`, configurable via `task_store_path`
3. **Scheduler as event bridge** — new "task-poll" schedule type checks for pending tasks each tick
4. **Fresh task context** — each task processed in its own Claude session (no session pollution)
5. **Internal DB for communication** — Telegram only for human-visible status updates
6. **Editable status messages** — one message per task, edited in-place as status changes
7. **Goal tracking** — tasks linked by goal_id for traceability and reflection
8. **Minimal metadata** — goal_id + context summary, grow as needed
9. **Organic collaboration** — minimal prompting, let agents discover when to delegate
10. **Weekly reflection** — capture task data for periodic learning (heartbeat or manual)

### Task Schema (Final)

```sql
tasks (
  id            TEXT PRIMARY KEY,        -- hex random ID
  chat_id       INTEGER NOT NULL,        -- originating chat
  goal_id       TEXT,                    -- links related tasks (nullable)
  from_agent    TEXT NOT NULL,           -- bot username of creator
  to_agent      TEXT NOT NULL,           -- bot username of target (can = from_agent for self-tasks)
  description   TEXT NOT NULL,
  context       TEXT DEFAULT '',         -- one-line context summary (invisible metadata)
  status        TEXT DEFAULT 'pending',  -- pending/working/completed/failed/canceled
  result        TEXT DEFAULT '',
  telegram_msg_id INTEGER DEFAULT 0,    -- for editable status messages
  created_at    DATETIME,
  updated_at    DATETIME,
  completed_at  DATETIME,
  ttl_minutes   INTEGER DEFAULT 60      -- auto-fail after TTL
)
```

### shell-task Skill Commands

```
shell-task create --to <agent|self> --description "..." [--goal <goal_id>] [--context "..."]
shell-task complete --id <task_id> --result "..."
shell-task fail --id <task_id> --reason "..."
shell-task list [--status pending|all]
shell-task status --id <task_id>
```

### Execution Flow

**Self-task (decomposition):**
1. Agent runs `shell-task create --to self --description "step 1: research X"`
2. Task written to shared DB
3. Agent's own scheduler tick detects pending self-task
4. Scheduler fires PromptFunc in fresh task context
5. Claude processes task, runs `shell-task complete --id X --result "..."`
6. Optional: Telegram status update in originating chat

**Delegation:**
1. Pikamini runs `shell-task create --to umbreon_mini_bot --description "verify Y" --context "I told mami to take ibuprofen"`
2. Task written to shared DB + Telegram status: "📋 pikamini → @umbreon: verify Y"
3. Umbreon's scheduler tick detects pending task for itself
4. Scheduler fires PromptFunc in fresh task context with task description + context
5. Umbreon processes, runs `shell-task complete --id X --result "looks correct"`
6. Telegram status edited: "✅ task completed by umbreon"
7. Pikamini's next context injection shows completed task + result

### Files to Implement

| File | Change |
|------|--------|
| `internal/transcript/taskstore.go` | New — standalone TaskStore with shared SQLite |
| `internal/scheduler/scheduler.go` | Add "task-poll" schedule type |
| `internal/rpc/server.go` | Expand `/task` or add `/task-delegate` endpoint |
| `internal/bridge/bridge.go` | Add taskStore, inject pending tasks, update groupAgentPrompt |
| `internal/daemon/daemon.go` | Open shared task store, wire to scheduler + RPC + bridge |
| `internal/config/config.go` | Add TaskStorePath |
| `internal/process/manager.go` | Add SHELL_AGENT_NAME / SHELL_BOT_USERNAME env vars |
| `skills/shell-task/SKILL.md` | Rewrite — full task management skill |
| `skills/shell-task/scripts/shell-task` | Rewrite — create/complete/fail/list/status |
| `internal/bridge/task_directives.go` | Remove or simplify (replaced by skill) |

---

## Round 8: Refinements & Internal Message Bus Design (2026-04-14)

### Refined Answers

**Task context contents**: Task goal, task metadata, and ghost memories relevant to the task. NOT raw conversation history — the agent gets task-specific context assembled from structured data.

**Reflection**: Separate heartbeat type (not part of regular heartbeat). A dedicated "reflection" heartbeat that aggregates task data and generates behavioral improvements. Could run weekly or every N heartbeats.

**TTL auto-fail**: Should notify via internal message bus, not Telegram. This is a system event, not a human-facing status change. The originating agent gets notified internally so it can decide what to do (retry, handle itself, inform the human).

### Finding 36: Internal Message Bus — The Missing Foundation

The user's answers reveal a pattern: task notifications, TTL failures, reflection triggers, and cross-agent events all need the same thing — **an internal event system that agents subscribe to**.

Today's architecture has no event bus. Everything is either:
- Synchronous: RPC call → response
- Scheduled: ticker fires every minute
- External: Telegram message delivery

What we need is a lightweight **event queue** that:
1. Multiple producers can write to (task system, scheduler, TTL checker, any agent)
2. Multiple consumers can read from (each agent's daemon)
3. Is persistent (survives restarts)
4. Is low-latency (seconds, not minutes)

### Finding 37: SQLite as the Event Bus

SQLite can serve as the event bus with minimal overhead:

```sql
events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  target      TEXT NOT NULL,        -- bot_username or "*" for broadcast
  event_type  TEXT NOT NULL,        -- "task.created", "task.completed", "task.failed", "task.ttl_expired", "reflection.due"
  payload     TEXT NOT NULL,        -- JSON: {task_id, chat_id, description, ...}
  created_at  DATETIME DEFAULT (datetime('now')),
  consumed_at DATETIME             -- NULL until consumed, then timestamp
)
CREATE INDEX idx_events_target ON events(target, consumed_at);
```

**Producer**: Any component writes an event row
**Consumer**: Each agent's scheduler polls for unconsumed events where `target = my_username OR target = "*"`
**Consumption**: Mark `consumed_at` on read, process the event

**Why this works:**
- SQLite WAL mode handles concurrent reads/writes from multiple processes
- Polling at 1-min scheduler tick is already there — just add an event check
- Events are persistent — survive daemon restarts
- Events are ordered — AUTOINCREMENT guarantees FIFO
- Events are auditable — never deleted, just marked consumed
- The same shared DB can hold both `tasks` and `events` tables

### Finding 38: Event-Driven Task Lifecycle

With the event bus, the task lifecycle becomes fully event-driven:

```
Task Created (by agent or scheduler):
  → INSERT into tasks table
  → INSERT event: {target: to_agent, type: "task.created", payload: {task_id, description}}
  → If cross-agent: INSERT event: {target: "telegram", type: "task.status", payload: {msg, chat_id}}

Agent's Scheduler Tick:
  → Poll events table for this agent
  → For each "task.created" event:
    → Fire PromptFunc in fresh task context
    → Context includes: task goal, metadata, ghost memories
  → For each "task.ttl_expired" event:
    → Notify originating agent (another event)
    → Optionally handle/retry

Task Completed:
  → UPDATE tasks table
  → INSERT event: {target: from_agent, type: "task.completed", payload: {task_id, result}}
  → INSERT event: {target: "telegram", type: "task.status", payload: {edit msg_id, new text}}

TTL Expiry (checked by scheduler):
  → UPDATE task status to "failed"
  → INSERT event: {target: from_agent, type: "task.ttl_expired", payload: {task_id}}
```

### Finding 39: Event Bus Enables Future Patterns

The event bus isn't just for tasks — it's a foundation for:

| Event Type | Producer | Consumer | Purpose |
|------------|----------|----------|---------|
| `task.created` | shell-task skill | target agent scheduler | Process delegated/self task |
| `task.completed` | shell-task skill | originating agent | Learn result, continue workflow |
| `task.ttl_expired` | TTL checker | originating agent | Handle timeout |
| `task.status` | any | Telegram handler | Edit status message |
| `reflection.due` | scheduler (weekly) | this agent | Run behavioral reflection |
| `skill.created` | agent | all agents | Share new skill availability |
| `memory.shared` | agent | target agent | Share specific knowledge |
| `agent.health` | daemon | monitoring | Heartbeat/health check |

This is the "internal system bus" the user asked for — a single SQLite table that all shell components can use for async communication.

### Finding 40: Reflection as Separate Heartbeat Type

Behavioral reflection should be its own schedule type, not piggybacked on regular heartbeat:

```
Schedule types:
  - cron: fires at cron expression
  - once: fires once at specified time
  - heartbeat: periodic check-in (existing)
  - task-poll: check for pending tasks (new)
  - reflection: periodic behavioral learning (new)
```

**Reflection heartbeat:**
- Cadence: weekly (or configurable, e.g., every 168 heartbeat ticks)
- Context: aggregated task data from the past week
  - Tasks created/completed/failed counts
  - Delegation patterns (who delegated to whom, for what)
  - Time-to-complete distribution
  - Self-task vs delegation ratio
  - Any human corrections or complaints post-task
- Output: behavioral learnings stored to ghost memory
- Model: could use opus for deeper reflection (model_routing: "reflection")

**Prompt structure:**
```
[Reflection] Weekly behavioral review

Task activity (past 7 days):
- Created: 12 tasks (8 self, 4 delegated)
- Completed: 10 (avg 3.2 min)
- Failed/expired: 2
- Delegation patterns: 3 to umbreon (verification), 1 to umbreon (translation)

Questions to reflect on:
1. Which delegations were valuable? Which were unnecessary?
2. Did self-decomposition improve task quality?
3. What patterns should you do more/less of?
4. Any task failures that could be prevented?

Store insights as procedural memories for future improvement.
```

---

## Final Consolidated Architecture (2026-04-14)

### Three New Components

**1. Shared Task Store** (`~/.shell/shared/tasks.db`)
- `tasks` table: unified self-tasks + delegation
- `events` table: internal message bus
- Both agents read/write via WAL mode

**2. shell-task Skill** (skill script → RPC → task store + events)
- create, complete, fail, list, status commands
- Writes tasks AND publishes events
- Available to all agents via global skills dir

**3. Scheduler Extensions** (new schedule types)
- `task-poll`: check events table for this agent, fire PromptFunc per pending task
- `reflection`: weekly behavioral learning from task data

### Task Context Assembly (what Claude sees in a fresh task session)

```
[Task] id=abc123, from=pikamini, goal=def456
Description: Verify that ibuprofen is safe with mami's current medications

Context: pikamini told mami to take ibuprofen for headache. Mami is currently taking Flonase.

[Relevant memories]
(ghost_search results for: "mami medications", "ibuprofen interactions", "Flonase")

[Instructions]
Process this task. When done, run: scripts/shell-task complete --id abc123 --result "your findings"
If you cannot complete it: scripts/shell-task fail --id abc123 --reason "why"
```

### Event Flow Diagram

```
                    ┌──────────────────────┐
                    │  ~/.shell/shared/    │
                    │  tasks.db            │
                    │  ┌─────────────────┐ │
                    │  │ tasks table     │ │
                    │  │ events table    │ │
                    │  └─────────────────┘ │
                    └──────┬───────┬───────┘
                           │       │
              read/write   │       │   read/write
                    ┌──────┘       └──────┐
                    │                     │
             ┌──────┴──────┐       ┌──────┴──────┐
             │  Pikamini   │       │  Umbreon    │
             │  Scheduler  │       │  Scheduler  │
             │             │       │             │
             │  tick loop: │       │  tick loop: │
             │  - heartbeat│       │  - heartbeat│
             │  - cron     │       │  - cron     │
             │  - task-poll│       │  - task-poll│
             │  - reflect  │       │  - reflect  │
             └──────┬──────┘       └──────┬──────┘
                    │                     │
             PromptFunc              PromptFunc
             (fresh ctx)             (fresh ctx)
                    │                     │
                    └──────┬──────────────┘
                           │
                    Telegram Group
                    (editable status msgs)
```

### Implementation Priority

| Phase | What | Why |
|-------|------|-----|
| 1 | TaskStore + events table | Foundation — everything depends on this |
| 2 | shell-task skill (create/complete/fail) | Agents need the tool to create tasks |
| 3 | Scheduler task-poll type | Tasks need to trigger agent processing |
| 4 | Fresh task context assembly | Agent needs proper context when processing |
| 5 | Telegram editable status messages | Human visibility |
| 6 | TTL checker + auto-fail events | Reliability |
| 7 | Heartbeat enrichment with task data | Learning loop (no separate reflection type — reuse existing heartbeat) |
| 8 | Prompt updates (groupAgentPrompt) | Encourage organic collaboration |

### Design Simplification: No Separate Reflection Schedule

Reflection reuses the existing heartbeat. The heartbeat enrichment (`enrichHeartbeatPrompt`) already aggregates context from all chats, runs memory maintenance, and stores learnings. Just add a "recent task activity" section to the heartbeat context when task data exists. The agent's existing reflection instincts (heartbeat-learning) handle pattern recognition and behavioral improvement organically. One fewer schedule type to build.

---

## Round 9: Scheduler Bug — Agents Can't Schedule From Heartbeat (2026-04-14)

### Finding 41: SHELL_CHAT_ID=0 Blocks Heartbeat Scheduling (CRITICAL BUG)

**The core problem:** When an agent runs during a heartbeat, `SHELL_CHAT_ID` is set to `0` (SystemChatID). The `/schedule` RPC endpoint rejects `chat_id=0` with "chat_id is required." This means **agents cannot create persistent schedules from within heartbeat processing.**

**The flow that fails:**
1. Heartbeat fires → `HandleMessageStreaming(ctx, 0, msg, "heartbeat", ...)`
2. Claude process spawned with `SHELL_CHAT_ID=0`
3. Agent decides to schedule a reminder → runs `scripts/shell-schedule cron --expr "0 21 * * *" --message "Flonase time!"`
4. Script sends `{"chat_id": 0, ...}` to RPC
5. RPC returns error: "chat_id is required"
6. Schedule NOT created

**Agent workarounds observed in logs:**
- Storing reminders in ghost memory, hoping heartbeat picks them up (unreliable)
- Using `CronCreate` (in-session, dies on restart)
- Asking papi to manually create the schedule
- Using shell_relay to send messages during heartbeat (works but not a schedule)

### Finding 42: Zero Successful shell-schedule Calls in Agent History

Searched both pikamini and umbreonmini message_map tables. Found **zero successful calls** to `scripts/shell-schedule`. Agents either:
- Don't know the skill exists (skill prompt may not be prominent enough)
- Tried and failed silently (chat_id=0 rejection)
- Used `[schedule]` text directives which are silently stripped

### Finding 43: The Fix — Allow Target Chat Specification

The scheduler needs to accept a **target chat_id** that's different from `SHELL_CHAT_ID`. When running in heartbeat (chat_id=0), the agent should be able to say "schedule this for mami's chat (832881763)."

**Options:**
1. **`--chat` flag on shell-schedule**: `shell-schedule cron --chat 832881763 --expr "0 21 * * *" --message "Flonase!"`
2. **Smart default**: If `SHELL_CHAT_ID=0` and agent specifies a chat_id in the request, use that. If `SHELL_CHAT_ID=0` and no chat specified, return helpful error instead of cryptic "chat_id required."
3. **Allow chat_id=0 for agent-level schedules**: A schedule with chat_id=0 fires in SystemChat (the agent's inner monologue), which can then relay to specific chats.

**Option 1 is cleanest** — explicit target chat. The agent knows which chat it wants to schedule for (it has the chat IDs in memory). The skill script just needs a `--chat` override.

### Finding 44: This Also Affects shell-task

The same SHELL_CHAT_ID=0 problem will affect shell-task during heartbeat processing. If an agent wants to create a self-task during heartbeat, `chat_id=0` would be passed. The task store needs to either:
- Accept chat_id=0 for agent-level tasks (tasks not scoped to a specific chat)
- Require explicit `--chat` flag like the scheduler fix
- Use a sentinel value for "agent-level" tasks

**Recommendation:** Tasks should support `chat_id=0` as "agent-level" (not chat-scoped). The task-poll scheduler can process these in SystemChat context. For delegation tasks, the creating agent should specify the target chat explicitly.

### Scheduler Fix Implementation

**shell-schedule script**: Add `--chat` flag that overrides `SHELL_CHAT_ID`:
```bash
# Current: CHAT_ID="${SHELL_CHAT_ID:-0}" (always uses env)
# Fixed: --chat flag takes precedence
CHAT_ID="${SHELL_CHAT_ID:-0}"
while [ $# -gt 0 ]; do
  case "$1" in
    --chat) CHAT_ID="$2"; shift 2 ;;
    ...
  esac
done
```

**RPC /schedule handler**: Improve error message when chat_id=0:
```
"chat_id is 0 (system chat). Use --chat <chat_id> to specify the target chat for this schedule."
```

**shell-task script**: Same pattern — `--chat` flag for explicit chat targeting.

**Skill prompt update**: Make it clear that agents should specify `--chat` when running from heartbeat context.

---

## Round 10: System Prompt & Skill Discoverability Audit (2026-04-14)

### Finding 45: System Prompt Assembly Order

The system prompt is built in this order:
1. **Agent Identity** (`cfg.Agent.SystemPrompt`) — personality/core instructions
2. **Memory Context** (`memory.SystemPrompt()`) — pinned ghost memories (3000 token budget)
3. **Current Time** (`timestampSystemPrompt()`) — when scheduler enabled
4. **Skills Section** (`skillsSystemPrompt()`) — catalog + bridge rules + playground
5. **Group Agent Prompt** (`groupAgentPrompt()`) — multi-agent awareness (if transcript enabled)

### Finding 46: Core vs Non-Core Skill Visibility

Skills have a `core: true` flag in frontmatter. Only 3 skills are core (full body always visible):
- **shell-remember** — memory storage
- **shell-heartbeat-log** — heartbeat inspection
- **shell-schedule** — scheduling

All other 12 skills are **non-core** — agents only see a one-liner description and must run `scripts/shell-skill load <name>` to see full instructions. This means:

| Skill | Core | Agent sees full instructions? |
|-------|------|-----|
| shell-schedule | Yes | Always — full SKILL.md in every prompt |
| shell-remember | Yes | Always |
| shell-heartbeat-log | Yes | Always |
| shell-task | No | Only one-liner — must lazy-load |
| shell-relay | No | Only one-liner |
| web-search | No | Only one-liner |
| generate-image | No | Only one-liner |
| browser | No | Only one-liner |
| ... | No | Only one-liner |

**Key insight**: shell-schedule IS core — agents DO see the full instructions. So the Flonase failure wasn't about discoverability. It was about agents choosing CronCreate (easier, in-session) over shell-schedule (requires Bash call, different syntax).

### Finding 47: The Real Scheduler Problem — CronCreate Is Too Easy

Agents default to CronCreate because:
1. It's a built-in Claude tool — no Bash needed, no script syntax to remember
2. shell-schedule requires: `scripts/shell-schedule cron --expr "0 21 * * *" --message "..." --mode prompt`
3. CronCreate just works immediately (until the session dies)

The agents KNOW shell-schedule exists (it's core, full body visible). They just prefer the path of least resistance.

**Fix options:**
1. Make CronCreate unavailable (remove from allowed_tools) — too aggressive
2. Add a bridge warning when CronCreate is used: "Warning: CronCreate is session-only. Use shell-schedule for persistent reminders."
3. Update the shell-schedule SKILL.md to emphasize it's the ONLY persistent option
4. Add to bridge rules: "ALWAYS use shell-schedule for reminders/recurring tasks. CronCreate dies on session restart."

### Finding 48: Bridge Rules Already Warn About Directives

The bridge rules in `skillsSystemPrompt()` already say:
```
Do NOT emit text directives like [schedule], [relay], etc. These are silently stripped.
For scheduling, use corresponding skill scripts via Bash.
```

But they DON'T mention CronCreate vs shell-schedule. This is the gap — agents need explicit guidance that CronCreate is ephemeral.

### Finding 49: Pinned Memory Reinforces Scheduling But Not Enough

Ghost memory has a pinned capability for scheduling:
```
[schedule cron="..." tz="America/Los_Angeles" mode="prompt"]...[/schedule]
→ persists to SQLite, survives session restarts, executed by shell daemon's 1-min tick loop
CronCreate tool → in-memory only, dies when Claude session ends, USELESS for persistent jobs
```

This is pinned and visible every turn. But the agents still use CronCreate — suggesting the memory is read but not strongly enough weighted in the agent's decision.

### Finding 50: System Prompt Improvements for shell-task

When we build shell-task, it should be **core** (full body always visible) because:
- It's foundational to the task decomposition and delegation system
- Agents need to see the full syntax without lazy-loading
- It replaces the current minimal shell-task skill

**Recommended core skills after implementation:**
- shell-remember (existing core)
- shell-schedule (existing core)
- shell-task (new, make core)
- shell-heartbeat-log (existing core)

### Concrete System Prompt Recommendations

**1. Add to bridge rules (prompt.go):**
```
IMPORTANT: CronCreate is SESSION-ONLY — it dies when your session restarts.
For ANY reminder, recurring task, or scheduled notification, ALWAYS use
scripts/shell-schedule. This is the ONLY way to create persistent schedules.
```

**2. Update shell-schedule SKILL.md — add prominent warning:**
```
⚠️ DO NOT use CronCreate for reminders — it dies on session restart.
This skill (shell-schedule) is the ONLY persistent scheduling mechanism.
```

**3. shell-task SKILL.md — mark as core:**
```yaml
---
name: shell-task
description: Create, complete, and manage tasks (self-decomposition and delegation)
core: true
allowed-tools: Bash
---
```

**4. Update groupAgentPrompt — reference shell-task instead of text directives:**
Replace `[task to=...]` directive syntax with:
```
To delegate work: scripts/shell-task create --to <agent> --description "..."
To complete a task: scripts/shell-task complete --id <id> --result "..."
```

**5. Add task-awareness to heartbeat enrichment (heartbeat.go):**
Include recent task activity from shared tasks.db alongside existing pending background tasks.
