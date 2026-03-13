package orchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// MCPServer implements a stdio-based MCP server that provides orchestration
// tools to the orchestrator Claude session.
type MCPServer struct {
	client            *Client
	orchestratorTitle string
}

// NewMCPServer creates a new MCP server backed by the claude-squad API.
func NewMCPServer(baseURL, orchestratorTitle string) *MCPServer {
	return &MCPServer{
		client:            NewClient(baseURL),
		orchestratorTitle: orchestratorTitle,
	}
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
	reader := bufio.NewReader(os.Stdin)
	writer := os.Stdout

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
			continue // skip malformed lines
		}

		resp := s.handleRequest(&req)
		if resp != nil {
			out, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			out = append(out, '\n')
			writer.Write(out)
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
		if inst.Parent == s.orchestratorTitle && inst.Title != s.orchestratorTitle {
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
