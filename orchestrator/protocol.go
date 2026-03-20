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
	// Prompt is set when the agent has an interactive prompt (e.g. permission request).
	Prompt *WatcherPanePrompt `json:"prompt,omitempty"`
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
		if a.Prompt != nil && len(a.Prompt.Actions) > 0 {
			b.WriteString("\n**Interactive prompt — needs your response:**\n")
			if a.Prompt.Message != "" {
				b.WriteString(fmt.Sprintf("Context: %s\n", a.Prompt.Message))
			}
			b.WriteString("Actions:\n")
			for _, act := range a.Prompt.Actions {
				b.WriteString(fmt.Sprintf("- %s (key: %q)\n", act.Label, act.Key))
			}
			b.WriteString("Use `respond_to_prompt` with agent=\"" + a.Name + "\" and the appropriate key.\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Use your MCP tools to take action: send_to_agent to dispatch work, read_agent_output to check results, respond_to_prompt to handle permission requests, or mark_task_done if complete.\n")
	return b.String()
}

// FormatForDisplay formats the status update with ANSI colors for terminal display.
// Agent output is dimmed and enclosed in a box to de-emphasize it.
func (u *StatusUpdate) FormatForDisplay() string {
	var b strings.Builder
	for _, a := range u.Agents {
		// Agent header in bold
		b.WriteString(colorize(ansiBold, fmt.Sprintf("  ┌─ %s [%s]", a.Name, a.Status)))
		b.WriteString("\n")
		if a.Output != "" {
			// Dim the agent output and indent with box border
			lines := strings.Split(a.Output, "\n")
			for _, line := range lines {
				b.WriteString(colorize(ansiBrightBlk, "  │ "+line))
				b.WriteString("\n")
			}
		}
		if a.Prompt != nil && len(a.Prompt.Actions) > 0 {
			b.WriteString(colorize(ansiYellow, "  │ ⚠ Interactive prompt — needs response"))
			b.WriteString("\n")
		}
		b.WriteString(colorize(ansiBrightBlk, "  └─"))
		b.WriteString("\n")
	}
	return b.String()
}
