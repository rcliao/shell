package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	pm "github.com/rcliao/shell-pm"
	tunnel "github.com/rcliao/shell-tunnel"
	"github.com/rcliao/shell/internal/store"
)

// SetTimezone sets the default timezone without enabling the scheduler.
func (b *Bridge) SetTimezone(tz string) {
	if tz != "" {
		b.schedulerTZ = tz
	}
}

// SetSchedulerConfig enables schedule commands and sets the default timezone.
func (b *Bridge) SetSchedulerConfig(enabled bool, tz string) {
	b.schedulerEnabled = enabled
	b.schedulerTZ = tz
	if b.schedulerTZ == "" {
		b.schedulerTZ = "UTC"
	}
}

// Schedule handles the /schedule command with subcommands: add, list, delete.
func (b *Bridge) Schedule(ctx context.Context, chatID int64, args string) (string, error) {
	if !b.schedulerEnabled {
		return "Scheduler is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		return "Usage: /schedule add|list|delete ...", nil
	}

	// Parse subcommand
	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "add":
		return b.scheduleAdd(chatID, rest)
	case "list":
		return b.scheduleList(chatID)
	case "delete":
		return b.scheduleDelete(chatID, rest)
	case "enable":
		return b.scheduleEnable(chatID, rest)
	case "pause", "disable":
		return b.schedulePause(chatID, rest)
	default:
		return "Unknown subcommand. Usage: /schedule add|list|delete|enable|pause ...", nil
	}
}

func (b *Bridge) scheduleAdd(chatID int64, args string) (string, error) {
	if args == "" {
		return "Usage: /schedule add [--prompt] [--tz <timezone>] \"<cron or datetime>\" <message>\n\nUnquoted aliases also work: /schedule add @daily Good morning", nil
	}

	mode := "notify"
	if strings.HasPrefix(args, "--prompt ") {
		mode = "prompt"
		args = strings.TrimPrefix(args, "--prompt ")
		args = strings.TrimSpace(args)
	}

	// Parse --tz flag
	tzOverride := ""
	if strings.Contains(args, "--tz ") {
		idx := strings.Index(args, "--tz ")
		after := args[idx+5:]
		tzParts := strings.SplitN(after, " ", 2)
		tzOverride = tzParts[0]
		// Remove --tz <tz> from args
		args = strings.TrimSpace(args[:idx] + " " + strings.TrimSpace(after[len(tzParts[0]):]))
	}

	// Extract quoted expression, @-prefixed alias, or error
	var expr, message string
	if strings.HasPrefix(args, "\"") {
		endQuote := strings.Index(args[1:], "\"")
		if endQuote == -1 {
			return "Missing closing quote for schedule expression.", nil
		}
		expr = args[1 : endQuote+1]
		message = strings.TrimSpace(args[endQuote+2:])
	} else if strings.HasPrefix(args, "@") {
		// Unquoted @alias: split on first space
		aliasParts := strings.SplitN(args, " ", 2)
		expr = aliasParts[0]
		if len(aliasParts) > 1 {
			message = strings.TrimSpace(aliasParts[1])
		}
	} else {
		return "Schedule expression must be quoted or use an @alias. Example: /schedule add \"*/5 * * * *\" My reminder", nil
	}

	if message == "" {
		return "Please provide a message after the schedule expression.", nil
	}

	tz := b.schedulerTZ
	if tzOverride != "" {
		if _, err := time.LoadLocation(tzOverride); err != nil {
			return fmt.Sprintf("Invalid timezone: %s", tzOverride), nil
		}
		tz = tzOverride
	}
	sched := &store.Schedule{
		ChatID:   chatID,
		Label:    message,
		Message:  message,
		Schedule: expr,
		Timezone: tz,
		Mode:     mode,
		Enabled:  true,
	}

	// Auto-detect type: try to parse as datetime first
	if t, err := time.Parse(time.RFC3339, expr); err == nil {
		sched.Type = "once"
		sched.NextRunAt = t.UTC()
	} else if t, err := time.Parse("2006-01-02T15:04:05", expr); err == nil {
		// Parse in the configured timezone
		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		sched.Type = "once"
		sched.NextRunAt = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc).UTC()
	} else {
		// Try as cron expression
		cronExpr, err := parseScheduleCron(expr)
		if err != nil {
			return fmt.Sprintf("Invalid schedule expression: %s", err), nil
		}
		sched.Type = "cron"
		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
		if nextRun.IsZero() {
			return "Could not compute next run time.", nil
		}
		sched.NextRunAt = nextRun
	}

	id, err := b.store.SaveSchedule(sched)
	if err != nil {
		return "", fmt.Errorf("save schedule: %w", err)
	}

	modeStr := ""
	if mode == "prompt" {
		modeStr = " (prompt mode)"
	}

	if sched.Type == "once" {
		return fmt.Sprintf("Scheduled #%d: %s at %s%s", id, message, sched.NextRunAt.Format("2006-01-02 15:04 UTC"), modeStr), nil
	}
	return fmt.Sprintf("Scheduled #%d: %s (%s) next: %s%s", id, message, expr, sched.NextRunAt.Format("2006-01-02 15:04 UTC"), modeStr), nil
}

func (b *Bridge) scheduleList(chatID int64) (string, error) {
	schedules, err := b.store.ListSchedules(chatID)
	if err != nil {
		return "", fmt.Errorf("list schedules: %w", err)
	}
	if len(schedules) == 0 {
		return "No schedules found.", nil
	}

	var sb strings.Builder
	sb.WriteString("**Schedules:**\n\n")
	for _, sc := range schedules {
		status := "enabled"
		if !sc.Enabled {
			status = "disabled"
		}
		modeTag := ""
		if sc.Mode == "prompt" {
			modeTag = " [prompt]"
		}
		if sc.Type == "once" {
			sb.WriteString(fmt.Sprintf("**#%d** %s — at %s (%s)%s\n",
				sc.ID, sc.Label, sc.NextRunAt.Format("2006-01-02 15:04 UTC"), status, modeTag))
		} else {
			sb.WriteString(fmt.Sprintf("**#%d** %s — `%s` next: %s (%s)%s\n",
				sc.ID, sc.Label, sc.Schedule, sc.NextRunAt.Format("2006-01-02 15:04 UTC"), status, modeTag))
		}
	}
	return sb.String(), nil
}

func (b *Bridge) scheduleDelete(chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		return "Usage: /schedule delete <id>", nil
	}
	if err := b.store.DeleteSchedule(chatID, id); err != nil {
		return fmt.Sprintf("Failed to delete: %s", err), nil
	}
	return fmt.Sprintf("Schedule #%d deleted.", id), nil
}

func (b *Bridge) scheduleEnable(chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		return "Usage: /schedule enable <id>", nil
	}
	sc, err := b.store.GetSchedule(chatID, id)
	if err != nil {
		return "", fmt.Errorf("get schedule: %w", err)
	}
	if sc == nil {
		return fmt.Sprintf("Schedule #%d not found.", id), nil
	}

	// Recompute next_run_at for cron types
	if sc.Type == "cron" {
		cronExpr, err := parseScheduleCron(sc.Schedule)
		if err != nil {
			return fmt.Sprintf("Failed to parse cron expression: %s", err), nil
		}
		loc, _ := time.LoadLocation(sc.Timezone)
		if loc == nil {
			loc = time.UTC
		}
		nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
		if nextRun.IsZero() {
			return "Could not compute next run time.", nil
		}
		lastRun := time.Time{}
		if sc.LastRunAt != nil {
			lastRun = *sc.LastRunAt
		}
		if err := b.store.UpdateScheduleNextRun(id, nextRun, lastRun); err != nil {
			return "", fmt.Errorf("update next run: %w", err)
		}
		sc.NextRunAt = nextRun
	}

	if err := b.store.EnableSchedule(id); err != nil {
		return "", fmt.Errorf("enable schedule: %w", err)
	}
	return fmt.Sprintf("Schedule #%d enabled. Next run: %s", id, sc.NextRunAt.Format("2006-01-02 15:04 UTC")), nil
}

func (b *Bridge) schedulePause(chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		return "Usage: /schedule pause <id>", nil
	}
	// Verify the schedule belongs to this chat
	sc, err := b.store.GetSchedule(chatID, id)
	if err != nil {
		return "", fmt.Errorf("get schedule: %w", err)
	}
	if sc == nil {
		return fmt.Sprintf("Schedule #%d not found.", id), nil
	}
	if err := b.store.DisableSchedule(id); err != nil {
		return "", fmt.Errorf("disable schedule: %w", err)
	}
	return fmt.Sprintf("Schedule #%d paused.", id), nil
}

// parseScheduleCron is a bridge-level wrapper that calls the scheduler's cron parser.
func parseScheduleCron(expr string) (interface{ Next(time.Time) time.Time }, error) {
	return schedulerParseCron(expr)
}

// schedulerParseCron is set by the daemon during initialization to avoid
// a direct import of the scheduler package from bridge.
var schedulerParseCron func(string) (interface{ Next(time.Time) time.Time }, error)

// SetCronParser sets the cron parsing function used by schedule commands.
func SetCronParser(fn func(string) (interface{ Next(time.Time) time.Time }, error)) {
	schedulerParseCron = fn
}

// Heartbeat manages the per-chat heartbeat: a periodic prompt that routes through
// Claude with full session context, like a check-in.
func (b *Bridge) Heartbeat(ctx context.Context, chatID int64, args string) (string, error) {
	if !b.schedulerEnabled {
		return "Scheduler is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		return b.heartbeatStatus(chatID)
	}

	// Parse subcommand
	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]

	switch sub {
	case "stop":
		return b.heartbeatStop(chatID)
	case "status":
		return b.heartbeatStatus(chatID)
	default:
		// Treat as: /heartbeat <interval> <message>
		return b.heartbeatSet(chatID, args)
	}
}

func (b *Bridge) heartbeatSet(chatID int64, args string) (string, error) {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "Usage: /heartbeat <interval> <message>\n\nExamples:\n" +
			"  /heartbeat 30m Check inbox and calendar\n" +
			"  /heartbeat 1h Review open PRs and summarize\n" +
			"  /heartbeat 15m Any new notifications?", nil
	}

	intervalStr := parts[0]
	message := strings.TrimSpace(parts[1])

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return fmt.Sprintf("Invalid interval %q. Use Go duration format: 15m, 1h, 30m, 2h30m", intervalStr), nil
	}
	if interval < 1*time.Minute {
		return "Heartbeat interval must be at least 1 minute.", nil
	}

	// Remove existing heartbeat for this chat (one per chat)
	if err := b.store.DeleteHeartbeat(chatID); err != nil {
		slog.Warn("failed to delete old heartbeat", "error", err)
	}

	tz := b.schedulerTZ
	nextRun := time.Now().Add(interval).UTC()

	sched := &store.Schedule{
		ChatID:    chatID,
		Label:     "Heartbeat: " + message,
		Message:   message,
		Schedule:  intervalStr, // store the duration string
		Timezone:  tz,
		Type:      "heartbeat",
		Mode:      "prompt", // always prompt mode
		NextRunAt: nextRun,
		Enabled:   true,
	}

	id, err := b.store.SaveSchedule(sched)
	if err != nil {
		return "", fmt.Errorf("save heartbeat: %w", err)
	}

	return fmt.Sprintf("Heartbeat #%d set: every %s\nMessage: %s\nNext: %s",
		id, intervalStr, message, nextRun.Format("2006-01-02 15:04 UTC")), nil
}

func (b *Bridge) heartbeatStop(chatID int64) (string, error) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		return "", fmt.Errorf("get heartbeat: %w", err)
	}
	if hb == nil {
		return "No active heartbeat.", nil
	}
	if err := b.store.DeleteHeartbeat(chatID); err != nil {
		return "", fmt.Errorf("delete heartbeat: %w", err)
	}
	return "Heartbeat stopped.", nil
}

func (b *Bridge) heartbeatStatus(chatID int64) (string, error) {
	hb, err := b.store.GetHeartbeat(chatID)
	if err != nil {
		return "", fmt.Errorf("get heartbeat: %w", err)
	}
	if hb == nil {
		return "No active heartbeat.\n\nUsage: /heartbeat <interval> <message>\nExample: /heartbeat 30m Check inbox and calendar", nil
	}

	status := "active"
	if !hb.Enabled {
		status = "paused"
	}
	lastRun := "never"
	if hb.LastRunAt != nil {
		lastRun = hb.LastRunAt.Format("2006-01-02 15:04 UTC")
	}

	return fmt.Sprintf("**Heartbeat #%d** (%s)\n"+
		"**Interval:** %s\n"+
		"**Message:** %s\n"+
		"**Next run:** %s\n"+
		"**Last run:** %s\n\n"+
		"Use `/heartbeat stop` to disable.",
		hb.ID, status, hb.Schedule, hb.Message,
		hb.NextRunAt.Format("2006-01-02 15:04 UTC"), lastRun), nil
}

// Task manages the background task queue for a chat.
func (b *Bridge) Task(ctx context.Context, chatID int64, args string) (string, error) {
	args = strings.TrimSpace(args)
	if args == "" {
		return b.taskList(chatID)
	}

	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]

	switch sub {
	case "list":
		return b.taskList(chatID)
	case "add":
		if len(parts) < 2 {
			return "Usage: /task add <description>", nil
		}
		return b.taskAdd(chatID, strings.TrimSpace(parts[1]))
	case "done":
		if len(parts) < 2 {
			return "Usage: /task done <id>", nil
		}
		return b.taskDone(chatID, strings.TrimSpace(parts[1]))
	case "delete":
		if len(parts) < 2 {
			return "Usage: /task delete <id>", nil
		}
		return b.taskDelete(chatID, strings.TrimSpace(parts[1]))
	default:
		// Treat the whole args as a task description for convenience
		return b.taskAdd(chatID, args)
	}
}

func (b *Bridge) taskList(chatID int64) (string, error) {
	tasks, err := b.store.PendingTasks(chatID)
	if err != nil {
		return "", fmt.Errorf("list tasks: %w", err)
	}
	if len(tasks) == 0 {
		return "No pending tasks.\n\nUsage: /task add <description>", nil
	}

	var sb strings.Builder
	sb.WriteString("**Pending Tasks:**\n")
	for _, t := range tasks {
		sb.WriteString(fmt.Sprintf("- #%d: %s (%s)\n", t.ID, t.Description, t.CreatedAt.Format("Jan 2 15:04")))
	}
	sb.WriteString("\nTasks are picked up by heartbeat automatically.")
	return sb.String(), nil
}

func (b *Bridge) taskAdd(chatID int64, description string) (string, error) {
	id, err := b.store.AddTask(chatID, description)
	if err != nil {
		return "", fmt.Errorf("add task: %w", err)
	}
	return fmt.Sprintf("Task #%d added: %s\nIt will be picked up by the next heartbeat.", id, description), nil
}

func (b *Bridge) taskDone(chatID int64, idStr string) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "Invalid task ID.", nil
	}
	if err := b.store.CompleteTask(id); err != nil {
		return "", fmt.Errorf("complete task: %w", err)
	}
	return fmt.Sprintf("Task #%d completed.", id), nil
}

func (b *Bridge) taskDelete(chatID int64, idStr string) (string, error) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return "Invalid task ID.", nil
	}
	if err := b.store.DeleteTask(chatID, id); err != nil {
		return "", fmt.Errorf("delete task: %w", err)
	}
	return fmt.Sprintf("Task #%d deleted.", id), nil
}

// PM manages background processes via user commands.
func (b *Bridge) PM(ctx context.Context, chatID int64, args string) (string, error) {
	if b.pmMgr == nil {
		return "Process manager is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		// Default: list
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "list"}), nil
	}

	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "list":
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "list"}), nil

	case "start":
		// /pm start <name> <command> [--dir <dir>]
		if rest == "" {
			return "Usage: /pm start <name> <command> [--dir <dir>]", nil
		}
		name, cmd, dir := parsePMStartArgs(rest)
		if name == "" || cmd == "" {
			return "Usage: /pm start <name> <command> [--dir <dir>]", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "start", Name: name, Command: cmd, Dir: dir}), nil

	case "stop":
		if rest == "" {
			return "Usage: /pm stop <name>", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "stop", Name: rest}), nil

	case "logs":
		if rest == "" {
			return "Usage: /pm logs <name>", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "logs", Name: rest}), nil

	case "remove":
		if rest == "" {
			return "Usage: /pm remove <name>", nil
		}
		return pm.Execute(ctx, b.pmMgr, pm.Directive{Action: "remove", Name: rest}), nil

	default:
		return "Usage: /pm list|start|stop|logs|remove\n\n" +
			"Examples:\n" +
			"  /pm list\n" +
			"  /pm start myserver node server.js --dir /path/to/app\n" +
			"  /pm logs myserver\n" +
			"  /pm stop myserver\n" +
			"  /pm remove myserver", nil
	}
}

// Tunnel manages HTTP tunnels via user commands.
func (b *Bridge) Tunnel(ctx context.Context, chatID int64, args string) (string, error) {
	if b.tunnelMgr == nil {
		return "Tunnel is not enabled.", nil
	}

	args = strings.TrimSpace(args)
	if args == "" {
		// Default: list
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "list"}), nil
	}

	parts := strings.SplitN(args, " ", 2)
	sub := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = strings.TrimSpace(parts[1])
	}

	switch sub {
	case "list":
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "list"}), nil
	case "start":
		if rest == "" {
			return "Usage: /tunnel start <port> [--protocol https]", nil
		}
		port, protocol := parseTunnelStartArgs(rest)
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "start", Port: port, Protocol: protocol}), nil
	case "stop":
		if rest == "" {
			return "Usage: /tunnel stop <port>", nil
		}
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "stop", Port: rest}), nil
	default:
		// Treat as port number for quick start: /tunnel 8080
		return tunnel.Execute(ctx, b.tunnelMgr, tunnel.Directive{Action: "start", Port: sub}), nil
	}
}

// parseTunnelStartArgs parses "<port> [--protocol <proto>]" from /tunnel start args.
func parseTunnelStartArgs(args string) (port, protocol string) {
	if idx := strings.Index(args, "--protocol "); idx >= 0 {
		after := args[idx+11:]
		protoParts := strings.SplitN(after, " ", 2)
		protocol = protoParts[0]
		args = strings.TrimSpace(args[:idx])
	}
	return strings.TrimSpace(args), protocol
}

// parsePMStartArgs parses "<name> <command> [--dir <dir>]" from /pm start args.
func parsePMStartArgs(args string) (name, cmd, dir string) {
	// Extract --dir flag if present.
	if idx := strings.Index(args, "--dir "); idx >= 0 {
		after := args[idx+6:]
		dirParts := strings.SplitN(after, " ", 2)
		dir = dirParts[0]
		args = strings.TrimSpace(args[:idx])
	}

	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		return "", "", ""
	}
	return parts[0], parts[1], dir
}
