package teams

import "time"

// Config holds the orchestrator control loop configuration.
type Config struct {
	// PollInterval is how often to check sub-agent statuses (default 2s).
	PollInterval time.Duration
	// BaseURL is the agent-manager web server URL (default http://localhost:8787).
	BaseURL string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		PollInterval: 2 * time.Second,
		BaseURL:      "http://localhost:8787",
	}
}
