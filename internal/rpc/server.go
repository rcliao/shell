// Package rpc provides a lightweight HTTP server on a Unix socket that exposes
// bridge capabilities (pm, tunnel, relay, schedule, memory, task) as JSON endpoints.
// Claude calls these via Bash skill scripts instead of embedding text directives.
package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	pm "github.com/rcliao/shell-pm"
	tunnel "github.com/rcliao/shell-tunnel"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/store"
	"github.com/rcliao/shell/internal/transcript"
)

// NotifyFunc sends a text message to a Telegram chat/topic. threadID is the
// Telegram forum topic ID (0 = main chat / no topic).
type NotifyFunc func(chatID, threadID int64, msg string)

// SendPhotoFunc sends a photo to a Telegram chat/topic.
type SendPhotoFunc func(chatID, threadID int64, data []byte, caption string)

// RelayToBridgeFunc routes a relay message through the bridge so Claude
// processes it and has it in its session history for the target chat/topic.
type RelayToBridgeFunc func(ctx context.Context, chatID, threadID int64, message string)

// CronParser parses a cron expression and returns something with a Next method.
type CronParser func(expr string) (interface{ Next(time.Time) time.Time }, error)

// SkillsReloadFunc reloads skills and returns the count loaded.
type SkillsReloadFunc func() (int, error)

// SkillsLoadFunc returns the full prompt body for a named skill.
type SkillsLoadFunc func(name string) (string, error)

// Server is the bridge RPC server listening on a Unix socket.
type Server struct {
	listener  net.Listener
	server    *http.Server
	sockPath  string
	pmMgr     *pm.Manager
	tunnelMgr *tunnel.Manager
	store     *store.Store
	memory    *memory.Memory
	taskStore *transcript.TaskStore // shared task store for delegation
	notify         NotifyFunc
	sendPhoto      SendPhotoFunc
	relayToBridge  RelayToBridgeFunc
	cronParse      CronParser
	skillsReload   SkillsReloadFunc
	skillsLoad     SkillsLoadFunc
	timezone       string
	botUsername    string // this agent's bot username
}

// Config holds the dependencies for the RPC server.
type Config struct {
	SocketPath    string
	PMMgr         *pm.Manager
	TunnelMgr     *tunnel.Manager
	Store         *store.Store
	Memory        *memory.Memory
	TaskStore     *transcript.TaskStore // shared task store
	Notify        NotifyFunc
	SendPhoto     SendPhotoFunc
	RelayToBridge RelayToBridgeFunc
	CronParse     CronParser
	SkillsReload  SkillsReloadFunc
	SkillsLoad    SkillsLoadFunc
	Timezone      string
	BotUsername   string // this agent's bot username
}

// DefaultSocketPath returns the default Unix socket path.
func DefaultSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".shell", "bridge.sock")
}

// New creates a new RPC server. Does not start listening.
func New(cfg Config) *Server {
	if cfg.SocketPath == "" {
		cfg.SocketPath = DefaultSocketPath()
	}
	return &Server{
		sockPath:  cfg.SocketPath,
		pmMgr:     cfg.PMMgr,
		tunnelMgr: cfg.TunnelMgr,
		store:     cfg.Store,
		memory:    cfg.Memory,
		taskStore: cfg.TaskStore,
		notify:        cfg.Notify,
		sendPhoto:     cfg.SendPhoto,
		relayToBridge: cfg.RelayToBridge,
		cronParse:     cfg.CronParse,
		skillsReload:  cfg.SkillsReload,
		skillsLoad:    cfg.SkillsLoad,
		timezone:  cfg.Timezone,
		botUsername: cfg.BotUsername,
	}
}

// SocketPath returns the path to the Unix socket.
func (s *Server) SocketPath() string {
	return s.sockPath
}

// Start begins listening on the Unix socket. Call in a goroutine.
func (s *Server) Start() error {
	// Remove stale socket file
	os.Remove(s.sockPath)

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(s.sockPath), 0755); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	ln, err := net.Listen("unix", s.sockPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", s.sockPath, err)
	}
	s.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pm", s.handlePM)
	mux.HandleFunc("POST /tunnel", s.handleTunnel)
	mux.HandleFunc("POST /relay", s.handleRelay)
	mux.HandleFunc("POST /schedule", s.handleSchedule)
	mux.HandleFunc("POST /memory", s.handleMemory)
	mux.HandleFunc("POST /task", s.handleTask)
	mux.HandleFunc("POST /skills-reload", s.handleSkillsReload)
	mux.HandleFunc("POST /skills-load", s.handleSkillsLoad)
	mux.HandleFunc("POST /heartbeat-log", s.handleHeartbeatLog)

	s.server = &http.Server{Handler: mux}
	slog.Info("rpc server starting", "socket", s.sockPath)
	if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.server.Shutdown(ctx)
	}
	os.Remove(s.sockPath)
}

// --- Handlers ---

// PMRequest is the JSON body for POST /pm.
type PMRequest struct {
	Action  string `json:"action"`  // start, stop, list, logs, remove
	Name    string `json:"name"`    // process name
	Command string `json:"command"` // shell command (for start)
	Dir     string `json:"dir"`     // working directory (optional)
}

func (s *Server) handlePM(w http.ResponseWriter, r *http.Request) {
	if s.pmMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "process manager not enabled")
		return
	}

	var req PMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	d := pm.Directive{
		Action:  req.Action,
		Name:    req.Name,
		Command: req.Command,
		Dir:     req.Dir,
	}
	if d.Action == "" && d.Command != "" {
		d.Action = "start"
	}
	if d.Action == "" {
		d.Action = "list"
	}

	// Use Background context: PM processes outlive the HTTP request.
	result := pm.Execute(context.Background(), s.pmMgr, d)
	writeJSON(w, map[string]string{"result": result})
}

// TunnelRequest is the JSON body for POST /tunnel.
type TunnelRequest struct {
	Action   string `json:"action"`   // start, stop, list
	Port     string `json:"port"`     // local port
	Protocol string `json:"protocol"` // http or https
}

func (s *Server) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnelMgr == nil {
		writeError(w, http.StatusServiceUnavailable, "tunnel manager not enabled")
		return
	}

	var req TunnelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	d := tunnel.Directive{
		Action:   req.Action,
		Port:     req.Port,
		Protocol: req.Protocol,
	}
	if d.Action == "" {
		d.Action = "start"
	}

	// Tunnel processes outlive the HTTP request but we need a timeout
	// for the startup phase (waiting for cloudflared to produce a URL).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := tunnel.Execute(ctx, s.tunnelMgr, d)
	writeJSON(w, map[string]string{"result": result})
}

// RelayRequest is the JSON body for POST /relay.
type RelayRequest struct {
	ChatID          int64  `json:"chat_id"`
	MessageThreadID int64  `json:"message_thread_id"` // Telegram forum topic ID (0 = main chat)
	Message         string `json:"message"`
	ImagePath       string `json:"image_path"` // optional: send photo from file path
}

func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	if s.notify == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications not configured")
		return
	}

	var req RelayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ChatID == 0 {
		writeError(w, http.StatusBadRequest, "chat_id is required")
		return
	}
	if req.Message == "" && req.ImagePath == "" {
		writeError(w, http.StatusBadRequest, "message or image_path is required")
		return
	}

	// Send photo if image_path is provided.
	if req.ImagePath != "" && s.sendPhoto != nil {
		data, err := os.ReadFile(req.ImagePath)
		if err != nil {
			writeError(w, http.StatusBadRequest, "failed to read image: "+err.Error())
			return
		}
		slog.Info("rpc: relaying photo", "to_chat_id", req.ChatID, "thread_id", req.MessageThreadID, "path", req.ImagePath, "caption_len", len(req.Message))
		s.sendPhoto(req.ChatID, req.MessageThreadID, data, req.Message)
		// Log to target chat's store so Claude has context (don't send text to Telegram).
		if s.store != nil {
			if sess, err := s.store.GetSession(req.ChatID, req.MessageThreadID); err == nil && sess != nil {
				s.store.LogMessage(sess.ID, "assistant", "[Relayed photo] "+req.Message)
			}
		}
		writeJSON(w, map[string]any{"ok": true, "type": "photo"})
		return
	}

	slog.Info("rpc: relaying message", "to_chat_id", req.ChatID, "thread_id", req.MessageThreadID, "len", len(req.Message))

	// Route through bridge so Claude's session for the target chat has context.
	// This sends to Telegram AND adds to Claude's conversation history.
	if s.relayToBridge != nil {
		s.relayToBridge(r.Context(), req.ChatID, req.MessageThreadID, req.Message)
	} else {
		s.notify(req.ChatID, req.MessageThreadID, req.Message)
	}
	writeJSON(w, map[string]any{"ok": true, "type": "text"})
}


// ScheduleRequest is the JSON body for POST /schedule.
type ScheduleRequest struct {
	ChatID  int64  `json:"chat_id"`
	Type    string `json:"type"`    // "once" or "cron"
	At      string `json:"at"`      // RFC3339 or local datetime (for type=once)
	Cron    string `json:"cron"`    // cron expression (for type=cron)
	Message string `json:"message"` // schedule message/label
	Mode    string `json:"mode"`    // "notify" or "prompt" (default: notify)
	TZ      string `json:"tz"`      // timezone override
}

func (s *Server) handleSchedule(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	var req ScheduleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ChatID == 0 || req.Message == "" {
		writeError(w, http.StatusBadRequest, "chat_id and message are required")
		return
	}

	tz := req.TZ
	if tz == "" {
		tz = s.timezone
	}
	mode := req.Mode
	if mode == "" {
		mode = "notify"
	}

	sched := &store.Schedule{
		ChatID:   req.ChatID,
		Label:    req.Message,
		Message:  req.Message,
		Timezone: tz,
		Mode:     mode,
		Enabled:  true,
	}

	switch req.Type {
	case "once":
		sched.Type = "once"
		sched.Schedule = req.At
		t, err := time.Parse(time.RFC3339, req.At)
		if err != nil {
			// Try local datetime format
			loc, _ := time.LoadLocation(tz)
			if loc == nil {
				loc = time.UTC
			}
			t, err = time.ParseInLocation("2006-01-02T15:04:05", req.At, loc)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid at time: "+req.At)
				return
			}
		}
		sched.NextRunAt = t.UTC()

	case "cron":
		if s.cronParse == nil {
			writeError(w, http.StatusServiceUnavailable, "scheduler not enabled")
			return
		}
		sched.Type = "cron"
		sched.Schedule = req.Cron
		cronExpr, err := s.cronParse(req.Cron)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid cron: "+err.Error())
			return
		}
		loc, _ := time.LoadLocation(tz)
		if loc == nil {
			loc = time.UTC
		}
		nextRun := cronExpr.Next(time.Now().In(loc)).UTC()
		if nextRun.IsZero() {
			writeError(w, http.StatusBadRequest, "cron expression has no next run time")
			return
		}
		sched.NextRunAt = nextRun

	default:
		writeError(w, http.StatusBadRequest, "type must be 'once' or 'cron'")
		return
	}

	id, err := s.store.SaveSchedule(sched)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save schedule: "+err.Error())
		return
	}

	writeJSON(w, map[string]any{
		"id":       id,
		"type":     sched.Type,
		"next_run": sched.NextRunAt.Format("2006-01-02 15:04 UTC"),
	})
}

// MemoryRequest is the JSON body for POST /memory.
type MemoryRequest struct {
	ChatID  int64  `json:"chat_id"`
	Action  string `json:"action"`  // "remember" or "heartbeat-learning"
	Content string `json:"content"` // memory content
	Kind    string `json:"kind"`    // "semantic", "episodic", "procedural" (default: semantic)
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if s.memory == nil {
		writeError(w, http.StatusServiceUnavailable, "memory not enabled")
		return
	}

	var req MemoryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	switch req.Action {
	case "remember":
		err := s.memory.StoreDirective(r.Context(), req.ChatID, req.Content, req.Kind)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store: "+err.Error())
			return
		}
		slog.Info("rpc: stored memory", "chat_id", req.ChatID, "kind", req.Kind, "len", len(req.Content))
		writeJSON(w, map[string]any{"ok": true})

	case "heartbeat-learning":
		err := s.memory.StoreHeartbeatLearning(r.Context(), req.ChatID, req.Content)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store: "+err.Error())
			return
		}
		slog.Info("rpc: stored heartbeat learning", "chat_id", req.ChatID, "len", len(req.Content))
		writeJSON(w, map[string]any{"ok": true})

	case "behavioral":
		err := s.memory.StoreBehavioralLearning(r.Context(), req.ChatID, req.Content)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store: "+err.Error())
			return
		}
		slog.Info("rpc: stored behavioral learning", "chat_id", req.ChatID, "len", len(req.Content))
		writeJSON(w, map[string]any{"ok": true})

	default:
		writeError(w, http.StatusBadRequest, "action must be 'remember', 'heartbeat-learning', or 'behavioral'")
	}
}

// TaskRequest is the JSON body for POST /task.
// Supports both legacy background tasks (numeric ID) and new delegated tasks (string ID).
type TaskRequest struct {
	ChatID      int64  `json:"chat_id"`
	Action      string `json:"action"`      // create, complete, fail, list, status, legacy_complete
	ID          int64  `json:"id"`          // legacy background task ID (numeric)
	TaskID      string `json:"task_id"`     // delegated task ID (string hex)
	To          string `json:"to"`          // target agent bot username (create)
	Description string `json:"description"` // task description (create)
	Context     string `json:"context"`     // context summary (create)
	GoalID      string `json:"goal_id"`     // parent goal ID (create)
	Result      string `json:"result"`      // result text (complete/fail)
	Reason      string `json:"reason"`      // failure reason (fail)
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	var req TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	switch req.Action {
	// --- Legacy background task completion (backward compat) ---
	case "complete":
		// If numeric ID is set, this is the old background task system.
		if req.ID > 0 {
			if s.store == nil {
				writeError(w, http.StatusServiceUnavailable, "store not available")
				return
			}
			if err := s.store.CompleteTask(req.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to complete task: "+err.Error())
				return
			}
			slog.Info("rpc: completed background task", "task_id", req.ID)
			writeJSON(w, map[string]any{"ok": true})
			return
		}
		// String task_id means new delegated task system.
		if req.TaskID == "" {
			writeError(w, http.StatusBadRequest, "task_id is required")
			return
		}
		if s.taskStore == nil {
			writeError(w, http.StatusServiceUnavailable, "task store not available")
			return
		}
		if err := s.taskStore.CompleteTask(req.TaskID, req.Result); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to complete task: "+err.Error())
			return
		}
		slog.Info("rpc: completed delegated task", "task_id", req.TaskID, "by", s.botUsername)
		// Notify originator via Telegram if this is a cross-agent task.
		if t, err := s.taskStore.GetTask(req.TaskID); err == nil && t != nil && t.FromAgent != t.ToAgent && s.notify != nil && t.ChatID != 0 {
			preview := req.Result
			if len(preview) > 80 {
				preview = preview[:80] + "..."
			}
			s.notify(t.ChatID, 0, fmt.Sprintf("✅ %s completed task %s: %s", s.botUsername, req.TaskID[:8], preview))
		}
		writeJSON(w, map[string]any{"ok": true, "task_id": req.TaskID})

	// --- New task system: create ---
	case "create":
		if s.taskStore == nil {
			writeError(w, http.StatusServiceUnavailable, "task store not available")
			return
		}
		if req.Description == "" {
			writeError(w, http.StatusBadRequest, "description is required")
			return
		}
		toAgent := req.To
		if toAgent == "" || toAgent == "self" {
			toAgent = s.botUsername
		}
		fromAgent := s.botUsername
		if fromAgent == "" {
			fromAgent = "unknown"
		}
		taskID, err := s.taskStore.CreateTask(transcript.Task{
			ChatID:      req.ChatID,
			GoalID:      req.GoalID,
			FromAgent:   fromAgent,
			ToAgent:     toAgent,
			Description: req.Description,
			Context:     req.Context,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create task: "+err.Error())
			return
		}
		slog.Info("rpc: created task", "task_id", taskID, "from", fromAgent, "to", toAgent, "description", req.Description)
		// Send Telegram notification for cross-agent delegation.
		if toAgent != fromAgent && s.notify != nil && req.ChatID != 0 {
			desc := req.Description
			if len(desc) > 80 {
				desc = desc[:80] + "..."
			}
			s.notify(req.ChatID, 0, fmt.Sprintf("📋 %s → @%s: %s", fromAgent, toAgent, desc))
		}
		writeJSON(w, map[string]any{"ok": true, "task_id": taskID})

	// --- New task system: fail ---
	case "fail":
		if s.taskStore == nil {
			writeError(w, http.StatusServiceUnavailable, "task store not available")
			return
		}
		if req.TaskID == "" {
			writeError(w, http.StatusBadRequest, "task_id is required")
			return
		}
		reason := req.Reason
		if reason == "" {
			reason = req.Result // accept either field
		}
		if err := s.taskStore.FailTask(req.TaskID, reason); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to fail task: "+err.Error())
			return
		}
		slog.Info("rpc: failed task", "task_id", req.TaskID, "reason", reason)
		if t, err := s.taskStore.GetTask(req.TaskID); err == nil && t != nil && t.FromAgent != t.ToAgent && s.notify != nil && t.ChatID != 0 {
			s.notify(t.ChatID, 0, fmt.Sprintf("❌ Task %s failed: %s", req.TaskID[:8], reason))
		}
		writeJSON(w, map[string]any{"ok": true, "task_id": req.TaskID})

	// --- New task system: list ---
	case "list":
		if s.taskStore == nil {
			writeError(w, http.StatusServiceUnavailable, "task store not available")
			return
		}
		agent := s.botUsername
		if agent == "" {
			agent = req.To
		}
		pending, err := s.taskStore.PendingTasksFor(agent)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to list tasks: "+err.Error())
			return
		}
		var items []map[string]any
		for _, t := range pending {
			items = append(items, map[string]any{
				"id":          t.ID,
				"from":        t.FromAgent,
				"to":          t.ToAgent,
				"description": t.Description,
				"status":      t.Status,
				"goal_id":     t.GoalID,
				"created_at":  t.CreatedAt.Format(time.RFC3339),
			})
		}
		writeJSON(w, map[string]any{"ok": true, "tasks": items, "count": len(items)})

	// --- New task system: status ---
	case "status":
		if s.taskStore == nil {
			writeError(w, http.StatusServiceUnavailable, "task store not available")
			return
		}
		if req.TaskID == "" {
			writeError(w, http.StatusBadRequest, "task_id is required")
			return
		}
		t, err := s.taskStore.GetTask(req.TaskID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to get task: "+err.Error())
			return
		}
		if t == nil {
			writeError(w, http.StatusNotFound, "task not found: "+req.TaskID)
			return
		}
		writeJSON(w, map[string]any{
			"ok":          true,
			"id":          t.ID,
			"from":        t.FromAgent,
			"to":          t.ToAgent,
			"description": t.Description,
			"context":     t.Context,
			"status":      t.Status,
			"result":      t.Result,
			"goal_id":     t.GoalID,
			"created_at":  t.CreatedAt.Format(time.RFC3339),
		})

	default:
		writeError(w, http.StatusBadRequest, "action must be 'create', 'complete', 'fail', 'list', or 'status'")
	}
}

func (s *Server) handleSkillsReload(w http.ResponseWriter, r *http.Request) {
	if s.skillsReload == nil {
		writeError(w, http.StatusServiceUnavailable, "skills reload not configured")
		return
	}
	count, err := s.skillsReload()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reload failed: "+err.Error())
		return
	}
	slog.Info("rpc: skills reloaded", "count", count)
	writeJSON(w, map[string]any{"ok": true, "count": count})
}

// SkillsLoadRequest is the JSON body for POST /skills-load.
type SkillsLoadRequest struct {
	Name string `json:"name"`
}

func (s *Server) handleSkillsLoad(w http.ResponseWriter, r *http.Request) {
	if s.skillsLoad == nil {
		writeError(w, http.StatusServiceUnavailable, "skills load not configured")
		return
	}
	var req SkillsLoadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	body, err := s.skillsLoad(req.Name)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "name": req.Name, "body": body})
}

// HeartbeatLogRequest is the JSON body for POST /heartbeat-log.
type HeartbeatLogRequest struct {
	Limit int  `json:"limit"` // number of exchanges to return (default 10)
	Full  bool `json:"full"`  // if true, return full content (no truncation)
}

// HeartbeatLogEntry represents one heartbeat exchange in the system chat.
type HeartbeatLogEntry struct {
	Timestamp string `json:"timestamp"`
	Role      string `json:"role"`
	Kind      string `json:"kind"` // "regular", "deep", or "" for assistant
	Content   string `json:"content"`
}

func (s *Server) handleHeartbeatLog(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	var req HeartbeatLogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Limit <= 0 {
		req.Limit = 10
	}

	// Find the system chat session (chat_id = 0, main thread)
	sess, err := s.store.GetSession(0, 0)
	if err != nil || sess == nil {
		writeJSON(w, map[string]any{
			"ok":      true,
			"entries": []HeartbeatLogEntry{},
			"note":    "no system chat session yet — heartbeats haven't fired",
		})
		return
	}

	// Get last N*2 messages (paired user/assistant) — limited to 200 total
	rawLimit := req.Limit * 2
	if rawLimit > 200 {
		rawLimit = 200
	}
	msgs, err := s.store.GetMessages(sess.ID, rawLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get messages: "+err.Error())
		return
	}

	truncLen := 300
	if req.Full {
		truncLen = 4000
	}

	entries := make([]HeartbeatLogEntry, 0, len(msgs))
	for _, m := range msgs {
		entry := HeartbeatLogEntry{
			Timestamp: m.CreatedAt.Format(time.RFC3339),
			Role:      m.Role,
		}
		content := m.Content
		if m.Role == "user" {
			if strings.Contains(content, "[Heartbeat:deep]") {
				entry.Kind = "deep"
			} else if strings.Contains(content, "[Heartbeat]") {
				entry.Kind = "regular"
			}
		}
		if len(content) > truncLen {
			content = content[:truncLen] + "..."
		}
		entry.Content = content
		entries = append(entries, entry)
	}

	writeJSON(w, map[string]any{
		"ok":      true,
		"entries": entries,
		"count":   len(entries),
	})
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
