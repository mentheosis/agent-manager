package main

import "time"

// Config holds the orchestrator control loop configuration.
type Config struct {
	// PollInterval is how often to check sub-agent statuses (default 2s).
	PollInterval time.Duration
	// BatchWindow is how long to wait for additional agents to finish
	// before sending a batched update to the orchestrator (default 5s).
	BatchWindow time.Duration
	// BaseURL is the agent-manager web server URL (default http://localhost:8787).
	BaseURL string
	// GroupTitle is the title of the loop instance this orchestrator manages.
	GroupTitle string
	// MCPPort is the port for the MCP HTTP server (0 for stdio mode).
	MCPPort int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval: 2 * time.Second,
		BatchWindow:  5 * time.Second,
		BaseURL:      "http://localhost:8787",
		MCPPort:      0, // stdio mode by default
	}
}
