package orchestration

import (
	"bufio"
	"encoding/json"
	"io"
)

// ServeMCP keeps stdout exclusively JSON-RPC. Tool policy belongs to each mode.
func ServeMCP(in io.Reader, out io.Writer, tools any, call func(string, json.RawMessage) (any, error)) error {
	reader := bufio.NewScanner(in)
	reader.Buffer(make([]byte, 4096), 1<<20)
	for reader.Scan() {
		line := reader.Bytes()
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		if req.ID == nil {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			response["result"] = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "agent-manager-queue", "version": "1"}}
		case "ping":
			response["result"] = map[string]any{}
		case "tools/list":
			response["result"] = map[string]any{"tools": tools}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			err := json.Unmarshal(req.Params, &p)
			var value any
			if err == nil {
				value, err = call(p.Name, p.Arguments)
			}
			if err != nil {
				response["result"] = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": err.Error()}}}
			} else {
				raw, _ := json.Marshal(value)
				response["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(raw)}}}
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "unknown method"}
		}
		if e := json.NewEncoder(out).Encode(response); e != nil {
			return e
		}
	}
	return reader.Err()
}
