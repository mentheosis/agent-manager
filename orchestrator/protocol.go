package orchestrator

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
	b.WriteString("Use your MCP tools to take action: send_to_agent to dispatch work, read_agent_output to check results, or mark_task_done if complete.\n")
	return b.String()
}
