package process

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseBidirectionalEvents_BasicFlow(t *testing.T) {
	// Simulate: control_response → system init → assistant (tool_use) → user (tool_result) → assistant (text) → result
	input := strings.Join([]string{
		`{"type":"control_response","response":{"subtype":"success","request_id":"init_1"}}`,
		`{"type":"system","subtype":"init","session_id":"sess-bi-1","tools":[]}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"echo hello"}}]},"session_id":"sess-bi-1"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"hello\n"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Done!"}]},"session_id":"sess-bi-1"}`,
		`{"type":"result","result":"Done!","session_id":"sess-bi-1"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	var deltas []string
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, func(delta string) {
		deltas = append(deltas, delta)
	})

	if result.Text != "Done!" {
		t.Errorf("expected result text 'Done!', got %q", result.Text)
	}
	if result.SessionID != "sess-bi-1" {
		t.Errorf("expected session ID 'sess-bi-1', got %q", result.SessionID)
	}
	if len(result.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(result.ToolCalls))
	}
	tc := result.ToolCalls[0]
	if tc.Name != "Bash" {
		t.Errorf("expected tool name 'Bash', got %q", tc.Name)
	}
	if tc.ID != "tu_1" {
		t.Errorf("expected tool ID 'tu_1', got %q", tc.ID)
	}
	if tc.Input["command"] != "echo hello" {
		t.Errorf("expected tool input command 'echo hello', got %v", tc.Input["command"])
	}
	if len(deltas) != 1 || deltas[0] != "Done!" {
		t.Errorf("unexpected deltas: %v", deltas)
	}
}

func TestParseBidirectionalEvents_StreamDeltas(t *testing.T) {
	// stream_event deltas (from --include-partial-messages)
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-2"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":" world"}}}`,
		`{"type":"result","result":"Hello world","session_id":"sess-2"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	var deltas []string
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, func(delta string) {
		deltas = append(deltas, delta)
	})

	if result.Text != "Hello world" {
		t.Errorf("expected 'Hello world', got %q", result.Text)
	}
	if len(deltas) != 2 {
		t.Fatalf("expected 2 deltas, got %d", len(deltas))
	}
	if deltas[0] != "Hello" || deltas[1] != " world" {
		t.Errorf("unexpected deltas: %v", deltas)
	}
}

func TestParseBidirectionalEvents_ControlRequest_CanUseTool(t *testing.T) {
	// CLI → SDK: can_use_tool permission request; we auto-allow via stdin
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-3"}`,
		`{"type":"control_request","request_id":"req_1_abc","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"},"tool_use_id":"tu_99"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"OK"}]}}`,
		`{"type":"result","result":"OK","session_id":"sess-3"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)

	if result.Text != "OK" {
		t.Errorf("expected 'OK', got %q", result.Text)
	}

	// Verify the control_response written to stdin
	stdinOutput := stdinBuf.String()
	if !strings.Contains(stdinOutput, `"type":"control_response"`) {
		t.Errorf("expected control_response in stdin, got %q", stdinOutput)
	}
	if !strings.Contains(stdinOutput, `"req_1_abc"`) {
		t.Errorf("expected request_id in response, got %q", stdinOutput)
	}

	// Parse the response to verify structure
	var resp stdinControlResponse
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdinOutput)), &resp); err != nil {
		t.Fatalf("failed to parse control_response: %v", err)
	}
	if resp.Response["subtype"] != "success" {
		t.Errorf("expected subtype 'success', got %v", resp.Response["subtype"])
	}
	innerResp, ok := resp.Response["response"].(map[string]any)
	if !ok {
		t.Fatal("expected nested response map")
	}
	if innerResp["behavior"] != "allow" {
		t.Errorf("expected behavior 'allow', got %v", innerResp["behavior"])
	}
}

func TestParseBidirectionalEvents_MultipleToolCalls(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-4"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"Read","input":{"path":"/tmp/x"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"file contents"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu_2","name":"Write","input":{"path":"/tmp/y","content":"new"}}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2","content":""}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Updated the file."}]}}`,
		`{"type":"result","result":"Updated the file.","session_id":"sess-4"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)

	if len(result.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Name != "Read" {
		t.Errorf("expected first tool 'Read', got %q", result.ToolCalls[0].Name)
	}
	if result.ToolCalls[1].Name != "Write" {
		t.Errorf("expected second tool 'Write', got %q", result.ToolCalls[1].Name)
	}
}

func TestParseBidirectionalEvents_KeepAlive(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"keep_alive"}`,
		`{"type":"system","subtype":"init","session_id":"sess-5"}`,
		`{"type":"keep_alive"}`,
		`{"type":"result","result":"done","session_id":"sess-5"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)

	if result.Text != "done" {
		t.Errorf("expected 'done', got %q", result.Text)
	}
	if result.SessionID != "sess-5" {
		t.Errorf("expected session 'sess-5', got %q", result.SessionID)
	}
}

func TestParseBidirectionalEvents_NilCallback(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"text"}]}}`,
		`{"type":"result","result":"text","session_id":"s1"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)
	if result.Text != "text" {
		t.Errorf("expected 'text', got %q", result.Text)
	}
}

func TestParseBidirectionalEvents_SkipsUnparseable(t *testing.T) {
	input := strings.Join([]string{
		`not json`,
		`{"type":"result","result":"ok","session_id":"s1"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)
	if result.Text != "ok" {
		t.Errorf("expected 'ok', got %q", result.Text)
	}
}

func TestParseBidirectionalEvents_PlainTextContent(t *testing.T) {
	// Content as a plain string (not array of blocks)
	input := strings.Join([]string{
		`{"type":"assistant","message":{"role":"assistant","content":"plain text response"}}`,
		`{"type":"result","result":"plain text response","session_id":"s1"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	var deltas []string
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, func(delta string) {
		deltas = append(deltas, delta)
	})

	if result.Text != "plain text response" {
		t.Errorf("expected 'plain text response', got %q", result.Text)
	}
	if len(deltas) != 1 || deltas[0] != "plain text response" {
		t.Errorf("unexpected deltas: %v", deltas)
	}
}

func TestParseBidirectionalEvents_UsageParsing(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-usage"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Hello"}]}}`,
		`{"type":"result","result":"Hello","session_id":"sess-usage","total_cost_usd":0.0123,"num_turns":3,"usage":{"input_tokens":1500,"output_tokens":500,"cache_creation_input_tokens":200,"cache_read_input_tokens":800}}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)

	if result.Text != "Hello" {
		t.Errorf("expected 'Hello', got %q", result.Text)
	}
	if result.Usage == nil {
		t.Fatal("expected non-nil Usage")
	}
	if result.Usage.InputTokens != 1500 {
		t.Errorf("expected 1500 input tokens, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 500 {
		t.Errorf("expected 500 output tokens, got %d", result.Usage.OutputTokens)
	}
	if result.Usage.CacheCreationInputTokens != 200 {
		t.Errorf("expected 200 cache creation tokens, got %d", result.Usage.CacheCreationInputTokens)
	}
	if result.Usage.CacheReadInputTokens != 800 {
		t.Errorf("expected 800 cache read tokens, got %d", result.Usage.CacheReadInputTokens)
	}
	if result.Usage.CostUSD != 0.0123 {
		t.Errorf("expected cost 0.0123, got %f", result.Usage.CostUSD)
	}
	if result.Usage.NumTurns != 3 {
		t.Errorf("expected 3 turns, got %d", result.Usage.NumTurns)
	}
}

func TestParseBidirectionalEvents_NoUsage(t *testing.T) {
	// Result without usage fields should have nil Usage
	input := strings.Join([]string{
		`{"type":"result","result":"ok","session_id":"s1"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)

	if result.Usage != nil {
		t.Errorf("expected nil Usage for result without usage data, got %+v", result.Usage)
	}
}

func TestParseBidirectionalEvents_UnhandledTypes(t *testing.T) {
	// tool_progress, rate_limit_event, etc. should be silently ignored
	input := strings.Join([]string{
		`{"type":"tool_progress","tool_use_id":"tu_1","tool_name":"Bash","elapsed_time_seconds":5}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"type":"result","result":"ok","session_id":"s1"}`,
	}, "\n")

	var stdinBuf bytes.Buffer
	result := parseBidirectionalEvents(strings.NewReader(input), &stdinBuf, nil)
	if result.Text != "ok" {
		t.Errorf("expected 'ok', got %q", result.Text)
	}
}

func TestWriteJSON(t *testing.T) {
	var buf bytes.Buffer
	err := writeJSON(&buf, stdinControlRequest{
		Type:      "control_request",
		RequestID: "test_1",
		Request:   map[string]any{"subtype": "initialize"},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := buf.String()
	if !strings.HasSuffix(output, "\n") {
		t.Error("expected trailing newline")
	}
	if !strings.Contains(output, `"type":"control_request"`) {
		t.Errorf("unexpected output: %s", output)
	}
}

func TestNewTextUserMessage(t *testing.T) {
	msg := newTextUserMessage("hello world", "sess-123")

	if msg.Type != "user" {
		t.Errorf("expected type 'user', got %q", msg.Type)
	}
	if msg.SessionID != "sess-123" {
		t.Errorf("expected session_id 'sess-123', got %q", msg.SessionID)
	}
	if msg.Message.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Message.Role)
	}
	if msg.ParentToolUseID != nil {
		t.Error("expected nil parent_tool_use_id")
	}

	// Content should be a JSON string
	var content string
	if err := json.Unmarshal(msg.Message.Content, &content); err != nil {
		t.Fatalf("expected string content, got error: %v", err)
	}
	if content != "hello world" {
		t.Errorf("expected content 'hello world', got %q", content)
	}

	// Verify full JSON shape
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	jsonStr := string(data)
	if !strings.Contains(jsonStr, `"parent_tool_use_id":null`) {
		t.Errorf("expected null parent_tool_use_id in JSON, got %s", jsonStr)
	}
}

func TestContentUnion_UnmarshalString(t *testing.T) {
	var c contentUnion
	if err := c.UnmarshalJSON([]byte(`"hello"`)); err != nil {
		t.Fatal(err)
	}
	if c.Text != "hello" {
		t.Errorf("expected text 'hello', got %q", c.Text)
	}
	if len(c.Blocks) != 0 {
		t.Errorf("expected no blocks, got %d", len(c.Blocks))
	}
}

func TestContentUnion_UnmarshalBlocks(t *testing.T) {
	var c contentUnion
	if err := c.UnmarshalJSON([]byte(`[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Bash","input":{"cmd":"ls"}}]`)); err != nil {
		t.Fatal(err)
	}
	if c.Text != "" {
		t.Errorf("expected no text, got %q", c.Text)
	}
	if len(c.Blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(c.Blocks))
	}
	if c.Blocks[0].Type != "text" || c.Blocks[0].Text != "hi" {
		t.Errorf("unexpected first block: %+v", c.Blocks[0])
	}
	if c.Blocks[1].Type != "tool_use" || c.Blocks[1].Name != "Bash" {
		t.Errorf("unexpected second block: %+v", c.Blocks[1])
	}
}

func TestNewUserMessage_TextOnly(t *testing.T) {
	req := AgentRequest{Text: "hello"}
	msg := newUserMessage(req, "sess-1")

	// No attachments → should be a plain string.
	var content string
	if err := json.Unmarshal(msg.Message.Content, &content); err != nil {
		t.Fatalf("expected string content, got error: %v", err)
	}
	if content != "hello" {
		t.Errorf("expected 'hello', got %q", content)
	}
}

func TestNewUserMessage_WithImage(t *testing.T) {
	req := AgentRequest{
		Text:   "what is this?",
		Images: []ImageAttachment{{Path: "/tmp/photo.jpg", Width: 800, Height: 600, Size: 50000}},
	}
	msg := newUserMessage(req, "")

	// With attachments → text includes file path metadata.
	var content string
	if err := json.Unmarshal(msg.Message.Content, &content); err != nil {
		t.Fatalf("expected string content, got error: %v", err)
	}
	if !strings.Contains(content, "[Attached image: /tmp/photo.jpg") {
		t.Errorf("expected image metadata, got %q", content)
	}
	if !strings.Contains(content, "800x600") {
		t.Errorf("expected dimensions, got %q", content)
	}
	if !strings.HasSuffix(content, "what is this?") {
		t.Errorf("expected text at end, got %q", content)
	}
}

func TestNewUserMessage_WithPDF(t *testing.T) {
	req := AgentRequest{
		Text: "summarize",
		PDFs: []PDFAttachment{{Path: "/tmp/doc.pdf", Size: 1048576}},
	}
	msg := newUserMessage(req, "sess-2")

	var content string
	if err := json.Unmarshal(msg.Message.Content, &content); err != nil {
		t.Fatalf("expected string content: %v", err)
	}
	if !strings.Contains(content, "[Attached PDF: /tmp/doc.pdf") {
		t.Errorf("expected PDF metadata, got %q", content)
	}
	if !strings.Contains(content, "1.0 MB") {
		t.Errorf("expected size, got %q", content)
	}
}
