package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// MCPServer implements an MCP server that provides orchestration
// tools to the leader Claude session.
type MCPServer struct {
	client     *Client
	groupTitle string
	logFunc    func(string)
	stateFunc  func() string // returns current loop state
	doneCh     chan string   // signals task completion with summary
	taskCh     chan string   // receives task from HTTP /task endpoint
	pauseFunc  func()
	resumeFunc func()
}

// NewMCPServer creates a new MCP server backed by the agent-manager API.
func NewMCPServer(baseURL, groupTitle string) *MCPServer {
	return &MCPServer{
		client:     NewClient(baseURL),
		groupTitle: groupTitle,
		logFunc:    func(s string) { fmt.Fprintln(os.Stderr, s) },
		stateFunc:  func() string { return "idle" },
		doneCh:     make(chan string, 1),
		taskCh:     make(chan string, 1),
	}
}

// DoneCh returns the channel that receives the summary when the leader signals task completion.
func (s *MCPServer) DoneCh() <-chan string {
	return s.doneCh
}

// SetStateFunc sets the function used to retrieve the loop's current state.
func (s *MCPServer) SetStateFunc(f func() string) {
	s.stateFunc = f
}

// TaskCh returns the channel that receives tasks submitted via the HTTP /task endpoint.
func (s *MCPServer) TaskCh() <-chan string {
	return s.taskCh
}

// SetPauseFunc sets the function called when pause is received via HTTP.
func (s *MCPServer) SetPauseFunc(f func()) {
	s.pauseFunc = f
}

// SetResumeFunc sets the function called when resume is received via HTTP.
func (s *MCPServer) SetResumeFunc(f func()) {
	s.resumeFunc = f
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

// Tool definitions - 5 tools for SDK-based orchestration (no respond_to_prompt)

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
		Description: "Read recent events from a specific sub-agent's history. Returns events in reverse chronological order (newest first).",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent": map[string]interface{}{
					"type":        "string",
					"description": "The agent name (title) to read from",
				},
				"count": map[string]interface{}{
					"type":        "integer",
					"description": "Number of events to return (default: 20, max: 200)",
					"default":     20,
				},
				"offset": map[string]interface{}{
					"type":        "integer",
					"description": "Skip N events from end before reading (for scrolling back)",
					"default":     0,
				},
				"types": map[string]interface{}{
					"type":        "string",
					"description": "Comma-separated event types to filter (e.g., 'assistant_text,tool_result')",
				},
			},
			"required": []string{"agent"},
		},
	},
	{
		Name:        "get_agent_status",
		Description: "Check the current status of a specific sub-agent (ready, running, error, creating)",
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
	{
		Name:        "mark_task_done",
		Description: "Signal that the overall task is complete. Call this when all sub-agents have finished and you have verified the results. This will pause the orchestration loop.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"summary": map[string]interface{}{
					"type":        "string",
					"description": "A summary of what was accomplished",
				},
			},
			"required": []string{"summary"},
		},
	},
}

// Run starts the MCP server, reading from stdin and writing to stdout.
func (s *MCPServer) Run() error {
	return s.serve(os.Stdin, os.Stdout)
}

// RunHTTP starts the MCP server as an HTTP server on the given port.
func (s *MCPServer) RunHTTP(port int) error {
	addr := fmt.Sprintf(":%d", port)
	s.log("Starting HTTP server on %s", addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/task", s.handleTask)
	mux.HandleFunc("/pause", s.handlePause)
	mux.HandleFunc("/resume", s.handleResume)
	mux.HandleFunc("/", s.handleHTTP)

	return http.ListenAndServe(addr, mux)
}

func (s *MCPServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"state": s.stateFunc(),
	})
}

func (s *MCPServer) handleTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if body.Task == "" {
		http.Error(w, "task is required", http.StatusBadRequest)
		return
	}
	s.log("Task received via HTTP: %d chars", len(body.Task))
	select {
	case s.taskCh <- body.Task:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *MCPServer) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.log("Pause received via HTTP")
	if s.pauseFunc != nil {
		s.pauseFunc()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *MCPServer) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.log("Resume received via HTTP")
	if s.resumeFunc != nil {
		s.resumeFunc()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
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
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if resp.Error != nil {
		s.log("Response error: %s", resp.Error.Message)
	}

	json.NewEncoder(w).Encode(resp)
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
					"name":    "agent-manager-orchestrator",
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
	case "mark_task_done":
		result, toolErr = s.toolMarkTaskDone(params.Arguments)
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
	children, err := s.client.GetChildren(s.groupTitle)
	if err != nil {
		return "", err
	}

	var agents []map[string]string
	for _, inst := range children {
		agents = append(agents, map[string]string{
			"name":   inst.Title,
			"status": inst.Status,
			"path":   inst.Path,
			"preset": inst.AgentPreset,
		})
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

	s.log("send_to_agent(%s, %d chars)", params.Agent, len(params.Prompt))
	if err := s.client.SendToInstance(params.Agent, params.Prompt); err != nil {
		return "", err
	}
	return fmt.Sprintf("Sent prompt to %s", params.Agent), nil
}

func (s *MCPServer) toolReadAgentOutput(args json.RawMessage) (string, error) {
	var params struct {
		Agent  string `json:"agent"`
		Count  int    `json:"count"`
		Offset int    `json:"offset"`
		Types  string `json:"types"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Apply defaults
	if params.Count == 0 {
		params.Count = 20
	}

	history, err := s.client.GetInstanceHistory(params.Agent, params.Count, params.Offset, params.Types)
	if err != nil {
		return "", err
	}

	s.log("read_agent_output(%s): %d events (total=%d, has_more=%v)",
		params.Agent, len(history.Events), history.TotalCount, history.HasMore)

	// Format events for the leader
	var output strings.Builder
	output.WriteString(fmt.Sprintf("Agent: %s\n", params.Agent))
	output.WriteString(fmt.Sprintf("Events: %d (total: %d, has_more: %v)\n\n", len(history.Events), history.TotalCount, history.HasMore))

	for i, event := range history.Events {
		output.WriteString(fmt.Sprintf("[%d] %s", i, event.Type))
		if event.IsError {
			output.WriteString(" (ERROR)")
		}
		output.WriteString("\n")

		// Include relevant content based on event type
		switch event.Type {
		case "assistant_text":
			output.WriteString(truncate(event.Text, 1000))
			output.WriteString("\n")
		case "tool_use":
			output.WriteString(fmt.Sprintf("Tool: %s\n", event.Name))
		case "tool_result":
			if event.IsError {
				output.WriteString(fmt.Sprintf("ERROR: %s\n", truncate(event.Output, 500)))
			} else {
				output.WriteString(truncate(event.Output, 500))
				output.WriteString("\n")
			}
		case "thinking":
			output.WriteString(truncate(event.Text, 500))
			output.WriteString("\n")
		case "error":
			output.WriteString(event.Text)
			output.WriteString("\n")
		}
		output.WriteString("\n")
	}

	return output.String(), nil
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

func (s *MCPServer) toolMarkTaskDone(args json.RawMessage) (string, error) {
	var params struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	s.log("mark_task_done: %s", params.Summary)

	select {
	case s.doneCh <- params.Summary:
	default:
	}

	return "Task marked as done. The orchestration loop will now pause.", nil
}

// truncate shortens a string to at most maxLen characters, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
