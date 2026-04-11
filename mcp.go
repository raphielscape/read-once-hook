package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Minimal JSON-RPC 2.0 structures for MCP
type rpcRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	Id      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	Jsonrpc string          `json:"jsonrpc"`
	Id      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func sendError(id json.RawMessage, code int, message string) {
	resp := rpcResponse{
		Jsonrpc: "2.0",
		Id:      id,
		Error: &rpcError{
			Code:    code,
			Message: message,
		},
	}
	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
}

func sendResult(id json.RawMessage, result any) {
	resp := rpcResponse{
		Jsonrpc: "2.0",
		Id:      id,
		Result:  result,
	}
	out, _ := json.Marshal(resp)
	fmt.Println(string(out))
}

func runMCP(cacheDir string) error {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			// Not a valid JSON-RPC request, ignore or send parse error
			continue
		}
		if req.Jsonrpc != "2.0" {
			continue
		}

		switch req.Method {
		case "initialize":
			// MCP initialize
			sendResult(req.Id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "read-once-hook",
					"version": "1.0.0",
				},
			})
		case "notifications/initialized":
			// ignore
		case "tools/list":
			sendResult(req.Id, map[string]any{
				"tools": []map[string]any{
					{
						"name":        "readOnceClearCache",
						"description": "Clear a file from the read-once hook cache. Use this when the file was evicted from your context and the hook is blocking you from reading it again.",
						"inputSchema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"filePath": map[string]any{
									"type":        "string",
									"description": "The absolute path of the file to clear from the cache",
								},
							},
							"required": []string{"filePath"},
						},
					},
				},
			})
		case "tools/call":
			var params struct {
				Name      string `json:"name"`
				Arguments struct {
					FilePath string `json:"filePath"`
				} `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				sendError(req.Id, -32602, "Invalid params")
				continue
			}

			if params.Name == "readOnceClearCache" {
				// We don't have a specific session ID from MCP, we must clear across all recent sessions
				// or clear the most recent one.
				// Wait, the hook uses the session ID passed to it. In Claude, we don't have that via MCP.
				// But we can just clear the file from *all* active sessions, or we can use the latest session lock file.
				// Let's implement clearFileGlobal in cache.go or here.
				err := clearFileGlobal(cacheDir, params.Arguments.FilePath)
				if err != nil {
					sendResult(req.Id, map[string]any{
						"content": []map[string]any{
							{
								"type": "text",
								"text": fmt.Sprintf("Error clearing cache: %v", err),
							},
						},
						"isError": true,
					})
				} else {
					sendResult(req.Id, map[string]any{
						"content": []map[string]any{
							{
								"type": "text",
								"text": "Cache cleared successfully. You can now read the file.",
							},
						},
					})
				}
			} else {
				sendError(req.Id, -32601, "Tool not found")
			}
		default:
			// ignore or send method not found
		}
	}
	return scanner.Err()
}
