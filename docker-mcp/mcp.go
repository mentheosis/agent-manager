package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// MCP JSON-RPC types. Mirrors the orchestrator package's wire format so the
// two servers stay consistent.

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPServer wires the JobManager to the MCP wire protocol.
type MCPServer struct {
	cfg     *Config
	jobs    *JobManager
	logFunc func(string)
}

func NewMCPServer(cfg *Config, jobs *JobManager) *MCPServer {
	return &MCPServer{
		cfg:     cfg,
		jobs:    jobs,
		logFunc: func(s string) { fmt.Fprintln(os.Stderr, s) },
	}
}

func (s *MCPServer) SetLogFunc(f func(string)) { s.logFunc = f }

func (s *MCPServer) log(format string, args ...interface{}) {
	s.logFunc(fmt.Sprintf("[mcp] "+format, args...))
}

// StreamingResponseWriter is passed to tool handlers that support streaming.
// If non-nil, the tool can write incremental output and return nil to indicate
// it handled the response itself.
type StreamingResponseWriter struct {
	W       http.ResponseWriter
	Flusher http.Flusher
}

// HandleRequest is the single entry point used by both the stdio and HTTP
// transports. For streaming support, use HandleRequestStreaming instead.
func (s *MCPServer) HandleRequest(req *jsonRPCRequest) *jsonRPCResponse {
	return s.HandleRequestStreaming(req, nil)
}

// HandleRequestStreaming handles a request with optional streaming support.
// If sw is non-nil and the tool supports streaming, output will be written
// incrementally to sw.W.
func (s *MCPServer) HandleRequestStreaming(req *jsonRPCRequest, sw *StreamingResponseWriter) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"tools": map[string]interface{}{},
				},
				"serverInfo": map[string]interface{}{
					"name":    "agent-manager-docker-mcp",
					"version": "0.1.0",
				},
			},
		}

	case "notifications/initialized":
		return nil

	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"tools": tools,
			},
		}

	case "tools/call":
		return s.handleToolCall(req, sw)

	default:
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error: &rpcError{
				Code:    -32601,
				Message: fmt.Sprintf("method not found: %s", req.Method),
			},
		}
	}
}

// ServeStdio runs the JSON-RPC loop on the provided reader/writer (one
// JSON object per line).
func (s *MCPServer) ServeStdio(r io.Reader, w io.Writer) error {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.log("malformed request: %v", err)
			continue
		}
		s.log("request: %s (id=%v)", req.Method, req.ID)
		resp := s.HandleRequest(&req)
		if resp == nil {
			continue
		}
		out, err := json.Marshal(resp)
		if err != nil {
			continue
		}
		if resp.Error != nil {
			s.log("error: %s", resp.Error.Message)
		}
		out = append(out, '\n')
		if _, err := w.Write(out); err != nil {
			return err
		}
	}
}

// errorResponse builds a tools/call error payload (note: tool errors are
// returned as result-with-isError, not as JSON-RPC errors, per MCP).
func toolErrorResponse(id interface{}, msg string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": "Error: " + msg},
			},
			"isError": true,
		},
	}
}

func toolTextResponse(id interface{}, text string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": text},
			},
		},
	}
}

func toolJSONResponse(id interface{}, payload interface{}) *jsonRPCResponse {
	out, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return toolErrorResponse(id, err.Error())
	}
	return toolTextResponse(id, string(out))
}
