package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

// ── MCP Protocol Types ──────────────────────────────────────────────────────

type toolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type mcpError struct {
	Message string
	Code    int // JSON-RPC error code
}

func (e *mcpError) Error() string { return e.Message }

func errInvalidRequest(msg string) *mcpError { return &mcpError{msg, -32600} }
func errMethodNotFound(msg string) *mcpError { return &mcpError{msg, -32601} }
func errInvalidParams(msg string) *mcpError  { return &mcpError{msg, -32602} }
func errToolNotFound(name string) *mcpError   { return &mcpError{"Unknown tool: " + name, -32601} }
func errToolError(msg string) *mcpError       { return &mcpError{msg, -32000} }
func errAuthError(msg string) *mcpError       { return &mcpError{msg, -32000} }

// ── MCP Server ──────────────────────────────────────────────────────────────

const maxInputBytes = 10 * 1024 * 1024
const maxResponseBytes = 50 * 1024 * 1024 // 50MB cap on JMAP response reads
const maxBatchIDs = 500                    // cap on IDs per destructive batch (well under JMAP maxObjectsInSet)

func run() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, maxInputBytes), maxInputBytes)

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg m
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}

		handleMessage(msg, enc)
	}
}

const LATEST_PROTOCOL_VERSION = "2024-11-05"

func handleMessage(msg m, enc *json.Encoder) {
	id := msg["id"]
	method := getString(msg, "method")
	params := getMap(msg, "params")

	send := func(result m) {
		if id == nil {
			return
		}
		enc.Encode(m{"jsonrpc": "2.0", "id": id, "result": result})
	}

	sendErr := func(code int, message string) {
		if id == nil {
			return // JSON-RPC: no response for notifications
		}
		enc.Encode(m{"jsonrpc": "2.0", "id": id, "error": m{"code": code, "message": message}})
	}

	switch method {
	case "initialize":
		clientVersion := getString(params, "protocolVersion")
		// Simple negotiation: if client requests a version, we'd ideally match it.
		// For now, we only support 2024-11-05.
		version := LATEST_PROTOCOL_VERSION
		if clientVersion != "" && clientVersion < LATEST_PROTOCOL_VERSION {
			version = clientVersion
		}

		send(m{
			"protocolVersion": version,
			"capabilities": m{
				"tools": m{"listChanged": false},
			},
			"serverInfo": m{"name": "fastmail-mcp", "version": version},
		})

	case "notifications/initialized":
		// Protocol: Client confirms it has received the initialize response.
		// We can use this to start background workers if needed.

	case "ping":
		send(m{})

	case "tools/list":
		toolList := make([]m, len(tools))
		for i, t := range tools {
			toolList[i] = m{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		}
		send(m{"tools": toolList})

	case "tools/call":
		params := getMap(msg, "params")
		toolName := getString(params, "name")
		if toolName == "" {
			sendErr(-32602, "Tool name missing")
			return
		}
		arguments := getMap(params, "arguments")
		if arguments == nil {
			arguments = m{}
		}

		result, err := callTool(toolName, arguments)
		if err != nil {
			if me, ok := err.(*mcpError); ok {
				sendErr(me.Code, me.Message)
			} else {
				sendErr(-32000, err.Error())
			}
			return
		}

		jsonBytes, err := json.Marshal(result)
		if err != nil {
			send(m{
				"content": []m{{"type": "text", "text": fmt.Sprintf("%v", result)}},
				"isError": false,
			})
			return
		}
		send(m{
			"content": []m{{"type": "text", "text": string(jsonBytes)}},
			"isError": false,
		})

	default:
		sendErr(-32601, "Method not found: "+method)
	}
}

// ── Tool Dispatch ───────────────────────────────────────────────────────────

func callTool(name string, arguments m) (any, error) {
	handler, ok := toolHandlers[name]
	if !ok {
		return nil, errToolNotFound(name)
	}
	return handler(arguments)
}

// ── Entry Point ─────────────────────────────────────────────────────────────

func main() {
	run()
}
