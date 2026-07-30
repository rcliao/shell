// Package mcp implements an MCP server that exposes bridge capabilities
// (process manager, tunnel) as first-class tools over stdio transport.
// Claude CLI connects to this server directly — no Bash scripts or RPC needed.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	gomcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const serverInstructions = `Shell bridge tools for managing background processes and tunnels.

Use shell_pm to start, stop, list, and manage background processes (servers, watchers, etc.).
CRITICAL: NEVER run long-running processes (servers, watchers) directly via Bash — always use shell_pm.

Use shell_tunnel to expose local ports to the internet via Cloudflare quick tunnels.

Typical web app workflow:
1. Write app files
2. shell_pm start → starts server in background
3. shell_tunnel start → exposes via public URL`

// Serve starts the MCP server on stdio, blocking until the connection closes.
// sockPath is the bridge RPC Unix socket used for PM and tunnel operations.
func Serve(ctx context.Context, sockPath string) error {
	server := gomcp.NewServer(&gomcp.Implementation{
		Name:    "shell",
		Version: "1.0.0",
	}, &gomcp.ServerOptions{
		Instructions: serverInstructions,
	})

	client := &rpcClient{sockPath: sockPath}
	registerTools(server, client)

	transport := &gomcp.StdioTransport{}
	return server.Run(ctx, transport)
}

// rpcClient makes HTTP requests to the bridge RPC server over a Unix socket.
type rpcClient struct {
	sockPath string
}

func (c *rpcClient) call(ctx context.Context, endpoint string, body any) (map[string]any, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", c.sockPath)
			},
		},
		Timeout: 60 * time.Second,
	}

	resp, err := httpClient.Post("http://bridge"+endpoint, "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("rpc call %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if errMsg, ok := result["error"].(string); ok {
		return nil, fmt.Errorf("%s", errMsg)
	}
	return result, nil
}

func registerTools(server *gomcp.Server, client *rpcClient) {
	// shell_pm — process manager
	server.AddTool(&gomcp.Tool{
		Name:        "shell_pm",
		Description: "Manage background processes. ALWAYS use this instead of running servers/watchers directly via Bash.",
		InputSchema: schema([]string{"action"}, map[string]map[string]any{
			"action":  prop("string", "Action: start, stop, list, logs, remove"),
			"name":    prop("string", "Process name (required for start/stop/logs/remove)"),
			"command": prop("string", "Shell command to run (required for start)"),
			"dir":     prop("string", "Working directory (optional, for start)"),
		}),
	}, func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var p struct {
			Action  string `json:"action"`
			Name    string `json:"name"`
			Command string `json:"command"`
			Dir     string `json:"dir"`
		}
		if err := unmarshalArgs(req, &p); err != nil {
			return errResult(err.Error()), nil
		}
		if p.Action == "" {
			p.Action = "list"
		}

		result, err := client.call(ctx, "/pm", map[string]any{
			"action":  p.Action,
			"name":    p.Name,
			"command": p.Command,
			"dir":     p.Dir,
		})
		if err != nil {
			return errResult(err.Error()), nil
		}
		return textResult(fmt.Sprintf("%v", result["result"])), nil
	})

	// shell_tunnel — HTTP tunnels
	server.AddTool(&gomcp.Tool{
		Name:        "shell_tunnel",
		Description: "Expose local ports to the internet via Cloudflare quick tunnels.",
		InputSchema: schema([]string{"action"}, map[string]map[string]any{
			"action":   prop("string", "Action: start, stop, list"),
			"port":     prop("string", "Local port to expose (required for start/stop)"),
			"protocol": prop("string", "Protocol: http (default) or https"),
		}),
	}, func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var p struct {
			Action   string `json:"action"`
			Port     string `json:"port"`
			Protocol string `json:"protocol"`
		}
		if err := unmarshalArgs(req, &p); err != nil {
			return errResult(err.Error()), nil
		}
		if p.Action == "" {
			p.Action = "start"
		}

		result, err := client.call(ctx, "/tunnel", map[string]any{
			"action":   p.Action,
			"port":     p.Port,
			"protocol": p.Protocol,
		})
		if err != nil {
			return errResult(err.Error()), nil
		}
		return textResult(fmt.Sprintf("%v", result["result"])), nil
	})

	// shell_schedule — create and manage schedules as a first-class tool.
	//
	// Previously reachable only through a Bash skill script, which gave the
	// agent no way to check whether its own schedule ever ran: a bad
	// expression or an auto-paused row stayed invisible until a human noticed
	// the reminder never arrived. describe closes that loop.
	server.AddTool(&gomcp.Tool{
		Name: "shell_schedule",
		Description: "Create, inspect and cancel scheduled reminders and jobs. " +
			"action=create needs message plus either at (one-shot) or cron. " +
			"action=list shows live schedules; action=describe id=N shows a schedule's next runs AND its recent run history — use it to confirm a schedule you created actually fires. " +
			"action=cancel id=N disables one; action=trigger id=N runs it on the next tick. " +
			"Creating the same schedule twice returns the existing one rather than a duplicate.",
		InputSchema: schema([]string{}, map[string]map[string]any{
			"action":  prop("string", "create (default), list, describe, cancel, trigger"),
			"id":      prop("integer", "Schedule id — required for describe, cancel, trigger"),
			"message": prop("string", "What to say or do when it fires (required for create)"),
			"at":      prop("string", "One-shot time, e.g. '2026-08-01 09:00' or '+30m' (create)"),
			"cron":    prop("string", "Cron expression for a recurring schedule (create)"),
			"mode":    prop("string", "notify = send the message verbatim; prompt = run it as a turn. Default notify."),
			"tz":      prop("string", "Timezone override; defaults to the agent's configured zone"),
			"chat_id": prop("integer", "Target chat; omit for the current chat"),
			"all":     prop("boolean", "list: include disabled and paused schedules"),
		}),
	}, func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var p struct {
			Action  string `json:"action"`
			ID      int64  `json:"id"`
			Message string `json:"message"`
			At      string `json:"at"`
			Cron    string `json:"cron"`
			Mode    string `json:"mode"`
			TZ      string `json:"tz"`
			ChatID  int64  `json:"chat_id"`
			All     bool   `json:"all"`
		}
		if err := unmarshalArgs(req, &p); err != nil {
			return errResult(err.Error()), nil
		}
		if p.Action == "" {
			p.Action = "create"
		}
		if p.ChatID == 0 {
			p.ChatID = currentChatID()
		}

		if p.Action == "create" {
			schedType := "cron"
			if p.Cron == "" {
				schedType = "once"
			}
			result, err := client.call(ctx, "/schedule", map[string]any{
				"chat_id": p.ChatID,
				"type":    schedType,
				"at":      p.At,
				"cron":    p.Cron,
				"message": p.Message,
				"mode":    p.Mode,
				"tz":      p.TZ,
			})
			if err != nil {
				return errResult(err.Error()), nil
			}
			return textResult(jsonText(result)), nil
		}

		result, err := client.call(ctx, "/schedules", map[string]any{
			"action":  p.Action,
			"id":      p.ID,
			"chat_id": p.ChatID,
			"all":     p.All,
		})
		if err != nil {
			return errResult(err.Error()), nil
		}
		return textResult(jsonText(result)), nil
	})

	// shell_relay — send messages/photos to other Telegram chats/topics
	server.AddTool(&gomcp.Tool{
		Name:        "shell_relay",
		Description: "Send a message or photo to a Telegram chat. Omit chat_id to send to the CURRENT chat (the one this conversation is happening in) — this is the safe default. Sending to a DIFFERENT chat requires both chat_id and cross_chat=true. Optional message_thread_id routes the reply into a specific forum topic (0 = main chat).",
		InputSchema: schema([]string{}, map[string]map[string]any{
			"chat_id":           prop("integer", "Target Telegram chat ID. Omit to send to the current chat."),
			"cross_chat":        prop("boolean", "Required (true) when chat_id is a different chat than the current one. Acknowledges the message is intentionally leaving this conversation."),
			"message_thread_id": prop("integer", "Forum topic ID (0 = main chat). Optional."),
			"message":           prop("string", "Text message or photo caption"),
			"image_path":        prop("string", "Path to image file to send as photo (optional)"),
		}),
	}, func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var p struct {
			ChatID          int64  `json:"chat_id"`
			CrossChat       bool   `json:"cross_chat"`
			MessageThreadID int64  `json:"message_thread_id"`
			Message         string `json:"message"`
			ImagePath       string `json:"image_path"`
		}
		if err := unmarshalArgs(req, &p); err != nil {
			return errResult(err.Error()), nil
		}
		// SHELL_CHAT_ID is the chat this Claude subprocess is serving
		// (0 / unset for heartbeat and system turns, which have no
		// current chat and must always target explicitly).
		currentChat, _ := strconv.ParseInt(os.Getenv("SHELL_CHAT_ID"), 10, 64)
		if p.ChatID == 0 {
			if currentChat == 0 {
				return errResult("chat_id is required (no current chat in this context)"), nil
			}
			p.ChatID = currentChat
		} else if currentChat != 0 && p.ChatID != currentChat && !p.CrossChat {
			return errResult(fmt.Sprintf("chat_id %d differs from the current chat %d — pass cross_chat=true if this is intentional, or omit chat_id to reply here", p.ChatID, currentChat)), nil
		}
		if p.Message == "" && p.ImagePath == "" {
			return errResult("message or image_path is required"), nil
		}

		result, err := client.call(ctx, "/relay", map[string]any{
			"chat_id":           p.ChatID,
			"message_thread_id": p.MessageThreadID,
			"message":           p.Message,
			"image_path":        p.ImagePath,
		})
		if err != nil {
			return errResult(err.Error()), nil
		}
		msgType := "text"
		if t, ok := result["type"].(string); ok {
			msgType = t
		}
		if p.MessageThreadID != 0 {
			return textResult(fmt.Sprintf("Relayed %s to chat %d topic %d", msgType, p.ChatID, p.MessageThreadID)), nil
		}
		return textResult(fmt.Sprintf("Relayed %s to chat %d", msgType, p.ChatID)), nil
	})
}

// --- Helpers ---

func schema(required []string, props map[string]map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

func unmarshalArgs(req *gomcp.CallToolRequest, v any) error {
	b, err := json.Marshal(req.Params.Arguments)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func textResult(text string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		Content: []gomcp.Content{&gomcp.TextContent{Text: text}},
	}
}

func errResult(msg string) *gomcp.CallToolResult {
	return &gomcp.CallToolResult{
		Content: []gomcp.Content{&gomcp.TextContent{Text: "error: " + msg}},
		IsError: true,
	}
}

// currentChatID is the chat this Claude subprocess is serving, exported into
// its environment by the daemon. Schedules default to it so an agent never has
// to guess a chat id — guessing is how a reminder ends up in the wrong chat.
func currentChatID() int64 {
	id, _ := strconv.ParseInt(os.Getenv("SHELL_CHAT_ID"), 10, 64)
	return id
}

// jsonText renders an RPC result as indented JSON. Schedule replies are
// structured (next runs, run history) and flattening them with %v turns them
// into an unreadable map dump.
func jsonText(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
