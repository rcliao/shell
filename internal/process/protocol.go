package process

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// ToolCall represents a tool invocation seen during streaming.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResult represents a tool execution result.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// --- Stdin types (SDK → CLI) ---

// stdinUserMessage is sent to the CLI to provide the user prompt.
// Matches SDKUserMessage from the Agent SDK.
type stdinUserMessage struct {
	Type            string         `json:"type"`               // "user"
	SessionID       string         `json:"session_id"`         // "" for first message, then session_id from system init
	Message         stdinMsgParam  `json:"message"`            // Anthropic API MessageParam
	ParentToolUseID *string        `json:"parent_tool_use_id"` // null for top-level messages
}

// stdinMsgParam is the Anthropic API MessageParam shape.
type stdinMsgParam struct {
	Role    string          `json:"role"`    // "user"
	Content json.RawMessage `json:"content"` // string or array of content blocks
}

// newTextUserMessage creates a stdinUserMessage with plain text content.
func newTextUserMessage(text, sessionID string) stdinUserMessage {
	content, _ := json.Marshal(text)
	return stdinUserMessage{
		Type:      "user",
		SessionID: sessionID,
		Message: stdinMsgParam{
			Role:    "user",
			Content: content,
		},
	}
}

// newUserMessage creates a stdinUserMessage for the bidirectional protocol.
// Attachments are referenced by file path in the text so Claude can use its
// Read tool to view them. This avoids the overhead of base64-encoding large
// files through stdin while keeping the attachment metadata strongly typed
// in AgentRequest (not baked into the text by the caller).
func newUserMessage(req AgentRequest, sessionID string) stdinUserMessage {
	text := FormatMessage(req)
	return newTextUserMessage(text, sessionID)
}


// stdinControlRequest is sent to the CLI to request an action (initialize, interrupt, etc.).
// Direction: SDK → CLI
type stdinControlRequest struct {
	Type      string         `json:"type"`       // "control_request"
	RequestID string         `json:"request_id"`
	Request   map[string]any `json:"request"`
}

// stdinControlResponse is sent to the CLI in response to the CLI's control_request
// (e.g. answering a can_use_tool permission check).
// Direction: SDK → CLI (in response to CLI's control_request on stdout)
type stdinControlResponse struct {
	Type     string         `json:"type"`     // "control_response"
	Response map[string]any `json:"response"`
}

// initRequestID is used for the initialize handshake.
const initRequestID = "init_1"

// --- Stdout types (CLI → SDK) ---

type innerEvent struct {
	Type  string       `json:"type"`
	Delta *streamDelta `json:"delta,omitempty"`
}

type streamDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// stdoutEvent is the union type for all NDJSON lines from the CLI's stdout.
// Uses json.RawMessage for polymorphic fields to avoid parsing everything upfront.
type stdoutEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Result    string          `json:"result,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
	Event     *innerEvent     `json:"event,omitempty"`     // stream_event: content_block_delta etc.
	Message   *stdoutMessage  `json:"message,omitempty"`   // assistant/user messages
	Request   json.RawMessage `json:"request,omitempty"`   // control_request body (CLI → SDK)
	Response  json.RawMessage `json:"response,omitempty"`  // control_response body (CLI → SDK, re: our init)
}

// stdoutMessage represents a message in assistant or user events.
// Content can be a string or array of content blocks.
type stdoutMessage struct {
	Role    string          `json:"role,omitempty"`
	Content contentUnion    `json:"content"`
}

// contentUnion handles the Anthropic API content field which can be
// either a plain string or an array of typed content blocks.
type contentUnion struct {
	Text   string         // set when content is a plain string
	Blocks []contentBlock // set when content is an array of blocks
}

func (c *contentUnion) UnmarshalJSON(data []byte) error {
	// Try string first (common for simple text responses)
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Text = s
		return nil
	}
	// Otherwise it's an array of content blocks
	return json.Unmarshal(data, &c.Blocks)
}

// contentBlock represents a single content block in an assistant or user message.
// This is a union type — which fields are populated depends on Type.
type contentBlock struct {
	Type string `json:"type"` // "text", "tool_use", "tool_result"

	// text block
	Text string `json:"text,omitempty"`

	// tool_use block
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// tool_result block
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// --- Wire helpers ---

// writeJSON encodes v as a JSON line to w.
func writeJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

// --- Event parsing ---

// parseBidirectionalEvents reads NDJSON from the CLI's stdout, handles control
// requests by writing responses to stdin, extracts tool calls, and calls onUpdate
// for text deltas. Returns the final SendResult.
//
// Stdout event types handled:
//   - system        → session_id extraction
//   - stream_event  → text deltas (content_block_delta)
//   - assistant      → complete messages with text/tool_use blocks
//   - user           → tool_result blocks (logged)
//   - control_request → CLI asking for permission (auto-allow via stdin)
//   - control_response → response to our initialize request (logged)
//   - result         → final result text
//   - keep_alive     → ignored
func parseBidirectionalEvents(r io.Reader, stdin io.Writer, onUpdate StreamFunc) SendResult {
	var result SendResult
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var event stdoutEvent
		if err := json.Unmarshal(line, &event); err != nil {
			slog.Debug("bidirectional: skip unparseable line", "error", err)
			continue
		}

		switch event.Type {
		case "system":
			if event.SessionID != "" {
				result.SessionID = event.SessionID
			}

		case "stream_event":
			if event.Event != nil && event.Event.Delta != nil && event.Event.Delta.Type == "text_delta" {
				if event.Event.Delta.Text != "" && onUpdate != nil {
					onUpdate(event.Event.Delta.Text)
				}
			}

		case "assistant":
			if event.SessionID != "" {
				result.SessionID = event.SessionID
			}
			if event.Message != nil {
				// Array content blocks: text + tool_use
				for _, block := range event.Message.Content.Blocks {
					switch block.Type {
					case "text":
						if block.Text != "" && onUpdate != nil {
							onUpdate(block.Text)
						}
					case "tool_use":
						result.ToolCalls = append(result.ToolCalls, ToolCall{
							ID:    block.ID,
							Name:  block.Name,
							Input: block.Input,
						})
						slog.Info("tool use", "name", block.Name, "id", block.ID)
					}
				}
				// Plain string content
				if event.Message.Content.Text != "" && onUpdate != nil {
					onUpdate(event.Message.Content.Text)
				}
			}

		case "user":
			// Tool results echoed by CLI
			if event.Message != nil {
				for _, block := range event.Message.Content.Blocks {
					if block.Type == "tool_result" {
						slog.Debug("bidirectional: tool_result", "tool_use_id", block.ToolUseID, "is_error", block.IsError)
					}
				}
			}

		case "control_request":
			// CLI → SDK: permission check or hook callback
			handleControlRequest(event, stdin)

		case "control_response":
			// CLI → SDK: response to our initialize/interrupt/etc. request
			slog.Debug("bidirectional: control_response received")

		case "result":
			result.Text = event.Result
			if event.SessionID != "" {
				result.SessionID = event.SessionID
			}
			return result

		case "keep_alive":
			// Ignore keep-alive pings

		default:
			// tool_progress, rate_limit_event, etc. — ignore for now
			slog.Debug("bidirectional: unhandled event type", "type", event.Type)
		}
	}

	return result
}

// handleControlRequest responds to a control_request from the CLI (stdout).
// Writes a control_response to stdin. Currently auto-allows all tool use.
func handleControlRequest(event stdoutEvent, stdin io.Writer) {
	var req map[string]any
	if err := json.Unmarshal(event.Request, &req); err != nil {
		slog.Warn("bidirectional: failed to parse control_request", "error", err)
		return
	}

	subtype, _ := req["subtype"].(string)
	slog.Debug("bidirectional: control_request", "subtype", subtype, "request_id", event.RequestID)

	resp := stdinControlResponse{
		Type: "control_response",
		Response: map[string]any{
			"subtype":    "success",
			"request_id": event.RequestID,
		},
	}

	if subtype == "can_use_tool" {
		resp.Response["response"] = map[string]any{"behavior": "allow"}
	}

	if err := writeJSON(stdin, resp); err != nil {
		slog.Warn("bidirectional: failed to write control_response", "error", err)
	}
}
