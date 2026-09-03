package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Text   string `json:"text"`
		Repeat int    `json:"repeat,omitempty"`
	} `json:"arguments"`
}

func main() {
	fmt.Fprintln(os.Stderr, "fake diagnostic: child started")

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	stdout := bufio.NewWriter(os.Stdout)
	defer stdout.Flush()

	for scanner.Scan() {
		response := respond(scanner.Bytes())
		if response == nil {
			continue
		}
		_, _ = stdout.Write(response)
		_ = stdout.WriteByte('\n')
		_ = stdout.Flush()
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "fake diagnostic: stdin: %v\n", err)
	}
}

func respond(frame []byte) []byte {
	var req request
	if err := json.Unmarshal(frame, &req); err != nil {
		return marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error": map[string]any{
				"code":    -32700,
				"message": "Parse error",
			},
		})
	}
	if len(req.ID) == 0 {
		return nil
	}

	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": "test"}
	case "tools/list":
		result = map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "Echoes text",
					"annotations": map[string]any{"readOnlyHint": true},
					"inputSchema": map[string]any{
						"type":     "object",
						"required": []string{"text"},
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
					},
				},
			},
		}
	case "tools/call":
		var params toolCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil || params.Name != "echo" {
			return errorResponse(req.ID, -32602, "Invalid params")
		}
		text := params.Arguments.Text
		if params.Arguments.Repeat > 0 {
			if params.Arguments.Repeat > 1024*1024 {
				return errorResponse(req.ID, -32602, "Invalid params")
			}
			text = strings.Repeat(text, params.Arguments.Repeat)
		}
		result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
		}
	default:
		return errorResponse(req.ID, -32601, "Method not found")
	}

	return marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(req.ID),
		"result":  result,
	})
}

func errorResponse(id json.RawMessage, code int, message string) []byte {
	return marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func marshal(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
