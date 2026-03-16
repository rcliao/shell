package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// HandleCommand processes a bot command.
func (b *Bridge) HandleCommand(ctx context.Context, chatID int64, cmd, args string) (string, error) {
	switch cmd {
	case "new":
		return b.Reset(ctx, chatID)
	case "status":
		return b.Status(chatID)
	case "help":
		return b.Help(), nil
	case "reactions":
		return b.Reactions(), nil
	case "start":
		return b.Start(ctx, chatID)
	case "remember":
		return b.Remember(ctx, chatID, args)
	case "forget":
		return b.Forget(ctx, chatID, args)
	case "memories":
		return b.ListMemories(ctx, chatID)
	case "review":
		return b.Review(ctx, chatID)
	case "correct":
		return b.Correct(ctx, chatID, args)
	case "plan":
		return b.Plan(ctx, chatID, args)
	case "planstatus":
		return b.PlanStatus(chatID)
	case "planstop":
		return b.PlanStop(chatID)
	case "planskip":
		return b.PlanSkip(chatID)
	case "planretry":
		return b.PlanRetry(ctx, chatID)
	case "schedule":
		return b.Schedule(ctx, chatID, args)
	case "heartbeat":
		return b.Heartbeat(ctx, chatID, args)
	case "task":
		return b.Task(ctx, chatID, args)
	case "pm":
		return b.PM(ctx, chatID, args)
	case "tunnel":
		return b.Tunnel(ctx, chatID, args)
	case "agent":
		return b.SwitchAgent(ctx, chatID, args)
	default:
		return fmt.Sprintf("Unknown command: /%s", cmd), nil
	}
}

// Start handles the /start command.
func (b *Bridge) Start(ctx context.Context, chatID int64) (string, error) {
	_, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return "Welcome to shell! Send me a message and I'll forward it to Claude Code.\n\nCommands:\n/new — Start a fresh session\n/status — Show session info\n/remember <text> — Remember something\n/forget <key> — Forget a memory\n/memories — List memories\n/review — Review all memories with summary\n/correct <n> <text> — Correct a memory by number\n/plan <goal> — Draft and run an autonomous plan\n/planstatus — Check plan progress\n/planstop — Cancel running plan\n/reactions — Show emoji reactions\n/help — Show help", nil
}

// Reset kills the current session and creates a fresh one.
func (b *Bridge) Reset(ctx context.Context, chatID int64) (string, error) {
	b.proc.Kill(chatID)
	if err := b.store.DeleteSession(chatID); err != nil {
		slog.Warn("failed to delete session from store", "error", err)
	}

	_, err := b.ensureSession(ctx, chatID)
	if err != nil {
		return "", err
	}
	return "Session reset. Starting fresh conversation.", nil
}

// Status returns info about the current session.
func (b *Bridge) Status(chatID int64) (string, error) {
	sess, err := b.store.GetSession(chatID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "No active session. Send a message to start one.", nil
	}

	msgs, err := b.store.GetMessages(sess.ID, 0)
	if err != nil {
		return "", err
	}

	procSess, _ := b.proc.Get(chatID)
	status := "active"
	if procSess != nil {
		status = string(procSess.Status)
	}

	return fmt.Sprintf(
		"## Status\n\n"+
			"**Session:** `%s`\n"+
			"**Status:** %s\n"+
			"**Messages:** %d\n"+
			"**Created:** %s\n"+
			"**Last active:** %s",
		sess.ProviderSessionID[:12]+"...",
		status,
		len(msgs),
		sess.CreatedAt.Format("2006-01-02 15:04:05"),
		sess.UpdatedAt.Format("2006-01-02 15:04:05"),
	), nil
}

func (b *Bridge) Help() string {
	help := "## shell\n\n" +
		"Telegram ↔ Claude Code bridge\n\n" +
		"Send any message to chat with Claude Code.\n\n" +
		"---\n\n" +
		"### Commands\n\n" +
		"- `/new` — Start a fresh conversation\n" +
		"- `/status` — Show current session info\n" +
		"- `/remember <text>` — Save a memory\n" +
		"- `/forget <key>` — Remove a stored memory\n" +
		"- `/memories` — List all stored memories\n" +
		"- `/review` — Review all memories with summary\n" +
		"- `/correct <n> <text>` — Correct a memory by number\n"

	if b.plan != nil {
		help += "\n### Plan execution\n\n" +
			"- `/plan <goal>` — Draft and run an autonomous plan\n" +
			"- `/planstatus` — Check plan progress\n" +
			"- `/planstop` — Cancel running plan\n" +
			"- `/planskip` — Skip blocked task, continue with next\n" +
			"- `/planretry` — Retry blocked task automatically\n"
	}

	if len(b.reactionMap) > 0 {
		help += "\n### Reactions\n\nReact to any message with:\n\n"
		// Sort by action name for stable output.
		type entry struct{ emoji, action string }
		entries := make([]entry, 0, len(b.reactionMap))
		for emoji, action := range b.reactionMap {
			entries = append(entries, entry{emoji, action})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].action < entries[j].action })
		for _, e := range entries {
			help += fmt.Sprintf("- %s → `%s`\n", e.emoji, e.action)
		}
	}

	if b.schedulerEnabled {
		help += "\n### Scheduler\n\n" +
			"- `/schedule add \"*/30 * * * *\" Reminder text` — Recurring notification\n" +
			"- `/schedule add @daily Good morning` — Unquoted alias (`@hourly`, `@daily`, `@weekly`, `@monthly`)\n" +
			"- `/schedule add --prompt \"0 9 * * 1-5\" Check PRs` — Recurring prompt (via Claude)\n" +
			"- `/schedule add --tz America/Los_Angeles \"0 9 * * *\" Check PRs` — Per-schedule timezone\n" +
			"- `/schedule add \"2026-03-10T09:00:00\" One-time reminder` — One-shot\n" +
			"- `/schedule list` — Show all schedules\n" +
			"- `/schedule enable <id>` — Re-enable a paused schedule\n" +
			"- `/schedule pause <id>` — Pause a schedule\n" +
			"- `/schedule delete <id>` — Remove a schedule\n" +
			"\n### Heartbeat\n\n" +
			"Periodic check-in that routes through Claude with session context (like cron but conversational).\n\n" +
			"- `/heartbeat 30m Check inbox and calendar` — Set heartbeat (one per chat)\n" +
			"- `/heartbeat` or `/heartbeat status` — Show current heartbeat\n" +
			"- `/heartbeat stop` — Stop the heartbeat\n" +
			"\n### Background Tasks\n\n" +
			"Queue tasks for heartbeat to pick up during its next check-in.\n\n" +
			"- `/task add <description>` — Add a background task\n" +
			"- `/task list` — Show pending tasks\n" +
			"- `/task done <id>` — Mark a task as completed\n" +
			"- `/task delete <id>` — Remove a task\n"
	}

	if b.pmMgr != nil {
		help += "\n### Process Manager\n\n" +
			"Manage background processes (web servers, watchers, etc.).\n\n" +
			"- `/pm` or `/pm list` — List managed processes\n" +
			"- `/pm start <name> <command> [--dir <dir>]` — Start a background process\n" +
			"- `/pm logs <name>` — View process logs\n" +
			"- `/pm stop <name>` — Stop a process\n" +
			"- `/pm remove <name>` — Remove a stopped process\n"
	}

	if b.tunnelMgr != nil {
		help += "\n### HTTP Tunnels\n\n" +
			"Expose local ports via Cloudflare quick tunnels.\n\n" +
			"- `/tunnel` or `/tunnel list` — List active tunnels\n" +
			"- `/tunnel start <port>` or `/tunnel <port>` — Start a tunnel\n" +
			"- `/tunnel stop <port>` — Stop a tunnel\n"
	}

	if b.pool != nil {
		help += "\n### Agents\n\n" +
			"- `/agent` — Show current agent\n" +
			"- `/agent <name>` — Switch to a different agent\n"
	}

	help += "\n---\n\n" +
		"`/reactions` — Show emoji→action mappings\n" +
		"`/help` — Show this help message"
	return help
}

// SwitchAgent handles the /agent command: show current agent or switch.
func (b *Bridge) SwitchAgent(ctx context.Context, chatID int64, args string) (string, error) {
	if b.pool == nil {
		return "Multi-agent not configured.", nil
	}

	args = strings.TrimSpace(args)

	// No args: show current agent and list available.
	if args == "" {
		current := b.pool.CurrentAgent(chatID)
		names := b.pool.AgentNames()
		sort.Strings(names)
		msg := fmt.Sprintf("Current agent: **%s**\n\nAvailable agents:\n", current)
		for _, name := range names {
			marker := ""
			if name == current {
				marker = " ← current"
			}
			msg += fmt.Sprintf("- `%s`%s\n", name, marker)
		}
		return msg, nil
	}

	// Switch agent.
	if !b.pool.Route(chatID, args) {
		return fmt.Sprintf("Agent %q not found. Use `/agent` to see available agents.", args), nil
	}

	// Kill current session so next message starts fresh with the new agent.
	b.proc.Kill(chatID)
	if err := b.store.DeleteSession(chatID); err != nil {
		slog.Warn("failed to delete session on agent switch", "error", err)
	}

	return fmt.Sprintf("Switched to agent **%s**. Starting fresh session.", args), nil
}
