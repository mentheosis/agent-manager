package orchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
)

// MCPServer implements an MCP server that provides orchestration
// tools to the leader Claude session.
type MCPServer struct {
	client     *Client
	groupTitle string
	logFunc    func(string)
}

// NewMCPServer creates a new MCP server backed by the claude-squad API.
func NewMCPServer(baseURL, groupTitle string) *MCPServer {
	return &MCPServer{
		client:     NewClient(baseURL),
		groupTitle: groupTitle,
		logFunc:    func(s string) { fmt.Println(s) },
	}
}

// SetLogFunc sets a custom log function.
func (s *MCPServer) SetLogFunc(f func(string)) {
	s.logFunc = f
}

func (s *MCPServer) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	s.logFunc(fmt.Sprintf("[MCP] %s", msg))
}

// JSON-RPC types for MCP protocol

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

// Tool definitions

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

var tools = []toolDef{
	{
		Name:        "list_agents",
		Description: "List all sub-agents with their current status, working directory, and preset type",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "send_to_agent",
		Description: "Send a prompt/task to a specific sub-agent",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent": map[string]interface{}{
					"type":        "string",
					"description": "The agent name (title) to send to",
				},
				"prompt": map[string]interface{}{
					"type":        "string",
					"description": "The prompt or task to send",
				},
			},
			"required": []string{"agent", "prompt"},
		},
	},
	{
		Name:        "read_agent_output",
		Description: "Read the latest output from a specific sub-agent",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent": map[string]interface{}{
					"type":        "string",
					"description": "The agent name (title) to read from",
				},
			},
			"required": []string{"agent"},
		},
	},
	{
		Name:        "get_agent_status",
		Description: "Check the current status of a specific sub-agent (ready, running, paused)",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent": map[string]interface{}{
					"type":        "string",
					"description": "The agent name (title) to check",
				},
			},
			"required": []string{"agent"},
		},
	},
}

// Run starts the MCP server, reading from stdin and writing to stdout.
func (s *MCPServer) Run() error {
	return s.serve(os.Stdin, os.Stdout)
}

// RunHTTP starts the MCP server as an HTTP server on the given port.
// Claude Code connects to this via "type": "http" in .mcp.json.
func (s *MCPServer) RunHTTP(port int) error {
	addr := fmt.Sprintf(":%d", port)
	s.log("Starting HTTP server on %s", addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleHTTP)

	return http.ListenAndServe(addr, mux)
}

func (s *MCPServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}

	s.log("Request: %s (id=%v)", req.Method, req.ID)
	resp := s.handleRequest(&req)

	w.Header().Set("Content-Type", "application/json")
	if resp == nil {
		// Notifications don't get a response, but HTTP needs something
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if resp.Error != nil {
		s.log("Response error: %s", resp.Error.Message)
	}

	json.NewEncoder(w).Encode(resp)
}

// RunOnSocket starts the MCP server on a Unix domain socket.
// It removes any stale socket file, listens for a single connection,
// and serves the MCP protocol over it.
func (s *MCPServer) RunOnSocket(socketPath string) error {
	// Clean up stale socket from a previous crash
	os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", socketPath, err)
	}
	defer listener.Close()
	defer os.Remove(socketPath)

	s.log("Listening on %s", socketPath)

	// Accept connections in a loop (one at a time — MCP is single-client)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("accept error: %w", err)
		}
		s.log("Client connected")
		s.serve(conn, conn)
		conn.Close()
		s.log("Client disconnected")
	}
}

// serve handles the JSON-RPC protocol on the given reader/writer.
func (s *MCPServer) serve(r io.Reader, w io.Writer) error {
	reader := bufio.NewReader(r)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("read error: %w", err)
		}

		var req jsonRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			s.log("Malformed request: %v", err)
			continue
		}

		s.log("Request: %s (id=%v)", req.Method, req.ID)
		resp := s.handleRequest(&req)
		if resp != nil {
			out, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			if resp.Error != nil {
				s.log("Response error: %s", resp.Error.Message)
			}
			out = append(out, '\n')
			w.Write(out)
		}
	}
}

func (s *MCPServer) handleRequest(req *jsonRPCRequest) *jsonRPCResponse {
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
					"name":    "claude-squad-orchestrator",
					"version": "1.0.0",
				},
			},
		}

	case "notifications/initialized":
		return nil // no response for notifications

	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"tools": tools,
			},
		}

	case "tools/call":
		return s.handleToolCall(req)

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

func (s *MCPServer) handleToolCall(req *jsonRPCRequest) *jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: "invalid params"},
		}
	}

	s.log("Tool call: %s", params.Name)

	var result string
	var toolErr error

	switch params.Name {
	case "list_agents":
		result, toolErr = s.toolListAgents()
	case "send_to_agent":
		result, toolErr = s.toolSendToAgent(params.Arguments)
	case "read_agent_output":
		result, toolErr = s.toolReadAgentOutput(params.Arguments)
	case "get_agent_status":
		result, toolErr = s.toolGetAgentStatus(params.Arguments)
	default:
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool: %s", params.Name)},
		}
	}

	if toolErr != nil {
		s.log("Tool %s error: %v", params.Name, toolErr)
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"content": []map[string]interface{}{
					{"type": "text", "text": fmt.Sprintf("Error: %v", toolErr)},
				},
				"isError": true,
			},
		}
	}

	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{"type": "text", "text": result},
			},
		},
	}
}

func (s *MCPServer) toolListAgents() (string, error) {
	instances, err := s.client.ListInstances()
	if err != nil {
		return "", err
	}

	var agents []map[string]string
	for _, inst := range instances {
		if inst.Parent == s.groupTitle && inst.Title != s.groupTitle {
			agents = append(agents, map[string]string{
				"name":   inst.Title,
				"status": inst.Status,
				"path":   inst.Path,
				"preset": inst.AgentPreset,
			})
		}
	}

	out, err := json.MarshalIndent(agents, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (s *MCPServer) toolSendToAgent(args json.RawMessage) (string, error) {
	var params struct {
		Agent  string `json:"agent"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if err := s.client.SendToInstance(params.Agent, params.Prompt); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent prompt to %s", params.Agent), nil
}

func (s *MCPServer) toolReadAgentOutput(args json.RawMessage) (string, error) {
	var params struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	history, err := s.client.GetInstanceHistory(params.Agent)
	if err != nil {
		return "", err
	}

	output := history.Pane
	if output == "" && len(history.StableLines) > 0 {
		start := len(history.StableLines) - 50
		if start < 0 {
			start = 0
		}
		output = fmt.Sprintf("%s", history.StableLines[start:])
	}
	return truncate(output, 4000), nil
}

func (s *MCPServer) toolGetAgentStatus(args json.RawMessage) (string, error) {
	var params struct {
		Agent string `json:"agent"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	status, err := s.client.GetInstanceStatus(params.Agent)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: %s", params.Agent, status), nil
}
