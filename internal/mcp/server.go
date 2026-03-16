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

	// shell_relay — send messages/photos to other Telegram chats
	server.AddTool(&gomcp.Tool{
		Name:        "shell_relay",
		Description: "Send a message or photo to another Telegram chat. Use this to forward info, send generated images, or communicate with other users.",
		InputSchema: schema([]string{"chat_id"}, map[string]map[string]any{
			"chat_id":    prop("integer", "Target Telegram chat ID"),
			"message":    prop("string", "Text message or photo caption"),
			"image_path": prop("string", "Path to image file to send as photo (optional)"),
		}),
	}, func(ctx context.Context, req *gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
		var p struct {
			ChatID    int64  `json:"chat_id"`
			Message   string `json:"message"`
			ImagePath string `json:"image_path"`
		}
		if err := unmarshalArgs(req, &p); err != nil {
			return errResult(err.Error()), nil
		}
		if p.ChatID == 0 {
			return errResult("chat_id is required"), nil
		}
		if p.Message == "" && p.ImagePath == "" {
			return errResult("message or image_path is required"), nil
		}

		result, err := client.call(ctx, "/relay", map[string]any{
			"chat_id":    p.ChatID,
			"message":    p.Message,
			"image_path": p.ImagePath,
		})
		if err != nil {
			return errResult(err.Error()), nil
		}
		msgType := "text"
		if t, ok := result["type"].(string); ok {
			msgType = t
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
