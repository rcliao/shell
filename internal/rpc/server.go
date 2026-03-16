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
	"time"

	pm "github.com/rcliao/shell-pm"
	tunnel "github.com/rcliao/shell-tunnel"
	"github.com/rcliao/shell/internal/memory"
	"github.com/rcliao/shell/internal/store"
)

// NotifyFunc sends a text message to a Telegram chat.
type NotifyFunc func(chatID int64, msg string)

// SendPhotoFunc sends a photo to a Telegram chat.
type SendPhotoFunc func(chatID int64, data []byte, caption string)

// CronParser parses a cron expression and returns something with a Next method.
type CronParser func(expr string) (interface{ Next(time.Time) time.Time }, error)

// Server is the bridge RPC server listening on a Unix socket.
type Server struct {
	listener  net.Listener
	server    *http.Server
	sockPath  string
	pmMgr     *pm.Manager
	tunnelMgr *tunnel.Manager
	store     *store.Store
	memory    *memory.Memory
	notify    NotifyFunc
	sendPhoto SendPhotoFunc
	cronParse CronParser
	timezone  string
}

// Config holds the dependencies for the RPC server.
type Config struct {
	SocketPath string
	PMMgr      *pm.Manager
	TunnelMgr  *tunnel.Manager
	Store      *store.Store
	Memory     *memory.Memory
	Notify     NotifyFunc
	SendPhoto  SendPhotoFunc
	CronParse  CronParser
	Timezone   string
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
		notify:    cfg.Notify,
		sendPhoto: cfg.SendPhoto,
		cronParse: cfg.CronParse,
		timezone:  cfg.Timezone,
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
	ChatID    int64  `json:"chat_id"`
	Message   string `json:"message"`
	ImagePath string `json:"image_path"` // optional: send photo from file path
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
		slog.Info("rpc: relaying photo", "to_chat_id", req.ChatID, "path", req.ImagePath, "caption_len", len(req.Message))
		s.sendPhoto(req.ChatID, data, req.Message)
		s.logRelay(req.ChatID, "[Relayed photo] "+req.Message)
		writeJSON(w, map[string]any{"ok": true, "type": "photo"})
		return
	}

	slog.Info("rpc: relaying message", "to_chat_id", req.ChatID, "len", len(req.Message))
	s.notify(req.ChatID, req.Message)
	s.logRelay(req.ChatID, req.Message)
	writeJSON(w, map[string]any{"ok": true, "type": "text"})
}

// logRelay logs a relayed message into the target chat's session so Claude
// has context when the recipient replies.
func (s *Server) logRelay(chatID int64, message string) {
	if s.store == nil {
		return
	}
	sess, err := s.store.GetSession(chatID)
	if err != nil || sess == nil {
		return
	}
	logMsg := "[Relay message sent to this chat]\n" + message
	if err := s.store.LogMessage(sess.ID, "assistant", logMsg); err != nil {
		slog.Warn("rpc: failed to log relay to target session", "chat_id", chatID, "error", err)
	}
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

	default:
		writeError(w, http.StatusBadRequest, "action must be 'remember' or 'heartbeat-learning'")
	}
}

// TaskRequest is the JSON body for POST /task.
type TaskRequest struct {
	ChatID int64  `json:"chat_id"`
	Action string `json:"action"` // "complete"
	ID     int64  `json:"id"`     // task ID
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "store not available")
		return
	}

	var req TaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	switch req.Action {
	case "complete":
		if req.ID == 0 {
			writeError(w, http.StatusBadRequest, "id is required")
			return
		}
		if err := s.store.CompleteTask(req.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to complete task: "+err.Error())
			return
		}
		slog.Info("rpc: completed task", "chat_id", req.ChatID, "task_id", req.ID)
		writeJSON(w, map[string]any{"ok": true})

	default:
		writeError(w, http.StatusBadRequest, "action must be 'complete'")
	}
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
