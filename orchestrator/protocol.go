package orchestrator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// StatusUpdate is sent from the control loop to the orchestrator session
// as a batched summary of agent status changes.
type StatusUpdate struct {
	// Agents contains the status of each agent that has changed since the last update.
	Agents []AgentStatus `json:"agents"`
	// Message is an optional human-readable context (e.g., "initial team setup").
	Message string `json:"message,omitempty"`
}

// AgentStatus represents the current state of a single sub-agent.
type AgentStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ready", "running", "paused", "error"
	// Output is the agent's latest output (truncated if very long).
	// Only populated when the agent has newly become "ready".
	Output string `json:"output,omitempty"`
}

// FormatForPrompt formats the status update as a human-readable prompt
// to send to the orchestrator Claude session.
func (u *StatusUpdate) FormatForPrompt() string {
	var b strings.Builder
	b.WriteString("## Agent Status Update\n\n")
	if u.Message != "" {
		b.WriteString(u.Message)
		b.WriteString("\n\n")
	}
	for _, a := range u.Agents {
		b.WriteString(fmt.Sprintf("### %s [%s]\n", a.Name, a.Status))
		if a.Output != "" {
			b.WriteString(a.Output)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("What are your next instructions? Respond with JSON commands.\n")
	return b.String()
}

// OrchestratorResponse is the structured response from the orchestrator session.
type OrchestratorResponse struct {
	Commands []Command `json:"commands"`
}

// Command is a single instruction from the orchestrator.
type Command struct {
	// Action is one of: "dispatch", "share_context", "done", "wait"
	Action string `json:"action"`
	// Agent is the target agent name (for "dispatch" and "get_status").
	Agent string `json:"agent,omitempty"`
	// Prompt is the task/message to send (for "dispatch").
	Prompt string `json:"prompt,omitempty"`
	// From is the source agent (for "share_context").
	From string `json:"from,omitempty"`
	// To is the destination agent (for "share_context").
	To string `json:"to,omitempty"`
	// Context is the information to share (for "share_context").
	Context string `json:"context,omitempty"`
	// Summary is the final result (for "done").
	Summary string `json:"summary,omitempty"`
}

// ParseOrchestratorResponse attempts to parse a JSON response from the orchestrator.
// It looks for a JSON block in the text, which may be wrapped in markdown code fences.
func ParseOrchestratorResponse(text string) (*OrchestratorResponse, error) {
	// Try to find JSON block in markdown code fences
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return nil, fmt.Errorf("no JSON commands found in orchestrator response")
	}

	var resp OrchestratorResponse
	if err := json.Unmarshal([]byte(jsonStr), &resp); err != nil {
		return nil, fmt.Errorf("failed to parse orchestrator commands: %w", err)
	}
	return &resp, nil
}

// extractJSON finds the first JSON object in text, handling markdown code fences.
func extractJSON(text string) string {
	// Try markdown code fence first
	if idx := strings.Index(text, "```json"); idx != -1 {
		start := idx + len("```json")
		if end := strings.Index(text[start:], "```"); end != -1 {
			return strings.TrimSpace(text[start : start+end])
		}
	}
	// Try plain code fence
	if idx := strings.Index(text, "```"); idx != -1 {
		start := idx + len("```")
		if end := strings.Index(text[start:], "```"); end != -1 {
			candidate := strings.TrimSpace(text[start : start+end])
			if strings.HasPrefix(candidate, "{") {
				return candidate
			}
		}
	}
	// Try to find raw JSON object
	if idx := strings.Index(text, `{"commands"`); idx != -1 {
		// Find matching closing brace
		depth := 0
		for i := idx; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return text[idx : i+1]
				}
			}
		}
	}
	return ""
}
