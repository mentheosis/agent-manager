package teams

import (
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
	Status string `json:"status"` // "ready", "running", "error", "creating"
	// HasError indicates if the agent has recent tool errors (permission issues, etc.)
	HasError bool `json:"has_error,omitempty"`
	// ErrorSummary is a brief description of the error if HasError is true.
	ErrorSummary string `json:"error_summary,omitempty"`
}

// FormatForPrompt formats the status update as a compact prompt for the leader.
func (u *StatusUpdate) FormatForPrompt() string {
	var b strings.Builder
	if u.Message != "" {
		b.WriteString(u.Message)
		b.WriteString("\n\n")
	}

	// List agent statuses compactly
	for _, a := range u.Agents {
		if a.HasError {
			b.WriteString(fmt.Sprintf("**%s** — %s (ERROR: %s)\n", a.Name, a.Status, a.ErrorSummary))
		} else {
			b.WriteString(fmt.Sprintf("- **%s**: %s\n", a.Name, a.Status))
		}
	}

	b.WriteString("\nUse your MCP tools: read_agent_output to check results, send_to_agent to dispatch work, or mark_task_done if complete.\n")
	return b.String()
}
