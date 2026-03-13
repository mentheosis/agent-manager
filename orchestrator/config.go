package orchestrator

import "time"

// Config holds the orchestrator control loop configuration.
type Config struct {
	// PollInterval is how often to check sub-agent statuses (default 2s).
	PollInterval time.Duration
	// BatchWindow is how long to wait for additional agents to finish
	// before sending a batched update to the orchestrator (default 5s).
	BatchWindow time.Duration
	// BaseURL is the claude-squad web server URL (default http://localhost:8080).
	BaseURL string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval: 2 * time.Second,
		BatchWindow:  5 * time.Second,
		BaseURL:      "http://localhost:8080",
	}
}
