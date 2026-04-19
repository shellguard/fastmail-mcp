package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// ── Tool count verification ─────────────────────────────────────────────────

func TestToolDefinitionsMatchHandlers(t *testing.T) {
	if len(tools) != len(toolHandlers) {
		t.Errorf("tool definitions (%d) != tool handlers (%d)", len(tools), len(toolHandlers))
	}

	for _, tool := range tools {
		if _, ok := toolHandlers[tool.Name]; !ok {
			t.Errorf("tool %q has a definition but no handler", tool.Name)
		}
	}

	for name := range toolHandlers {
		found := false
		for _, tool := range tools {
			if tool.Name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("handler %q has no tool definition", name)
		}
	}
}

func TestToolCount(t *testing.T) {
	if len(tools) != 72 {
		t.Errorf("expected 72 tools, got %d", len(tools))
	}
}

func TestToolNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range tools {
		if seen[tool.Name] {
			t.Errorf("duplicate tool name: %s", tool.Name)
		}
		seen[tool.Name] = true
	}
}

func TestAllToolsHaveFmPrefix(t *testing.T) {
	for _, tool := range tools {
		if len(tool.Name) < 3 || tool.Name[:3] != "fm_" {
			t.Errorf("tool %q does not have fm_ prefix", tool.Name)
		}
	}
}

func TestAllToolsHaveDescription(t *testing.T) {
	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
	}
}

func TestAllToolsHaveInputSchema(t *testing.T) {
	for _, tool := range tools {
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Errorf("tool %q has nil/invalid inputSchema", tool.Name)
			continue
		}
		if schema["type"] != "object" {
			t.Errorf("tool %q inputSchema type is not 'object'", tool.Name)
		}
	}
}

// ── callTool dispatch ───────────────────────────────────────────────────────

func TestCallTool_UnknownTool(t *testing.T) {
	_, err := callTool("fm_nonexistent", m{})
	if err == nil {
		t.Error("expected error for unknown tool")
	}
	mcpErr, ok := err.(*mcpError)
	if !ok {
		t.Fatalf("expected *mcpError, got %T", err)
	}
	if mcpErr.Code != -32601 {
		t.Errorf("expected code -32601, got %d", mcpErr.Code)
	}
}

// ── handleMessage ───────────────────────────────────────────────────────────

func captureMessage(msg m) m {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	handleMessage(msg, enc)

	var result m
	json.Unmarshal(buf.Bytes(), &result)
	return result
}

func TestHandleMessage_Initialize(t *testing.T) {
	resp := captureMessage(m{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  m{},
	})

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result map, got %T", resp["result"])
	}
	if result["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocol version: got %v", result["protocolVersion"])
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("expected serverInfo map")
	}
	if serverInfo["name"] != "fastmail-mcp" {
		t.Errorf("server name: got %v", serverInfo["name"])
	}
}

func TestHandleMessage_Initialize_Negotiation(t *testing.T) {
	// Client requests an older version
	resp := captureMessage(m{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": m{
			"protocolVersion": "2024-10-01",
		},
	})

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result map")
	}
	// Server should agree to the older version if it supports it (simulated here by accepting any < latest)
	if result["protocolVersion"] != "2024-10-01" {
		t.Errorf("expected negotiated version 2024-10-01, got %v", result["protocolVersion"])
	}
}

func TestHandleMessage_Ping(t *testing.T) {
	resp := captureMessage(m{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "ping",
	})
	if resp["error"] != nil {
		t.Errorf("ping should not return error: %v", resp["error"])
	}
}

func TestHandleMessage_ToolsList(t *testing.T) {
	resp := captureMessage(m{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/list",
	})

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatal("expected result map")
	}
	toolList, ok := result["tools"].([]any)
	if !ok {
		t.Fatal("expected tools array")
	}
	if len(toolList) != 72 {
		t.Errorf("expected 72 tools, got %d", len(toolList))
	}
}

func TestHandleMessage_UnknownMethod(t *testing.T) {
	resp := captureMessage(m{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "unknown/method",
	})

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error for unknown method")
	}
	if errObj["code"] != float64(-32601) {
		t.Errorf("expected -32601, got %v", errObj["code"])
	}
}

func TestHandleMessage_Notification_NoResponse(t *testing.T) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	// Notification = no "id" field → should produce no output
	handleMessage(m{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}, enc)

	if buf.Len() != 0 {
		t.Errorf("notification should produce no output, got: %s", buf.String())
	}
}

func TestHandleMessage_ToolsCall_MissingName(t *testing.T) {
	resp := captureMessage(m{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params":  m{"arguments": m{}},
	})

	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatal("expected error for missing tool name")
	}
	if errObj["code"] != float64(-32602) {
		t.Errorf("expected -32602, got %v", errObj["code"])
	}
}

// ── mcpError ────────────────────────────────────────────────────────────────

func TestMCPError_Codes(t *testing.T) {
	tests := []struct {
		err  *mcpError
		code int
	}{
		{errInvalidRequest("bad"), -32600},
		{errMethodNotFound("bad"), -32601},
		{errInvalidParams("bad"), -32602},
		{errToolNotFound("bad"), -32601},
		{errToolError("bad"), -32000},
		{errAuthError("bad"), -32000},
	}
	for _, tt := range tests {
		if tt.err.Code != tt.code {
			t.Errorf("%v: got code %d, want %d", tt.err, tt.err.Code, tt.code)
		}
	}
}

func TestMCPError_Message(t *testing.T) {
	err := errToolNotFound("my_tool")
	if err.Error() != "Unknown tool: my_tool" {
		t.Errorf("message: got %q", err.Error())
	}
}
