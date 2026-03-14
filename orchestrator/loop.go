package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// LoopState represents the current state of the control loop.
type LoopState int

const (
	LoopStateIdle    LoopState = iota // Waiting for user to start/restart
	LoopStateRunning                  // Actively orchestrating
	LoopStatePaused                   // Paused by user
	LoopStateDone                     // Orchestrator signaled done, waiting for restart
)

func (s LoopState) String() string {
	switch s {
	case LoopStateIdle:
		return "idle"
	case LoopStateRunning:
		return "running"
	case LoopStatePaused:
		return "paused"
	case LoopStateDone:
		return "done"
	default:
		return "unknown"
	}
}

// Loop is the control loop that drives the orchestrator.
// It subscribes to agent status changes via websocket and feeds batched
// results to the orchestrator session.
type Loop struct {
	config Config
	client *Client

	// groupTitle is the title of the loop instance itself (the parent of all children).
	groupTitle string
	// leaderTitle is the title of the leader/orchestrator Claude session (discovered dynamically).
	leaderTitle string
	// agentTitles are the titles of the sub-agent sessions (excluding the leader).
	agentTitles []string

	// state tracks the loop's current state.
	state LoopState
	mu    sync.RWMutex

	// watcher receives status updates from the web server via websocket.
	watcher *StatusWatcher

	// logFunc is called for each log line. Defaults to fmt.Println.
	logFunc func(string)

	// pauseCh is used to signal pause/resume.
	pauseCh chan struct{}
	// restartCh is used to signal a restart from idle/done state.
	restartCh chan string // carries optional new task prompt
}

// NewLoop creates a new control loop. groupTitle is the title of the loop instance
// (the parent). The leader is discovered dynamically among its children.
func NewLoop(cfg Config, groupTitle string) *Loop {
	logFunc := func(s string) { fmt.Println(s) }
	return &Loop{
		config:    cfg,
		client:    NewClient(cfg.BaseURL),
		groupTitle: groupTitle,
		watcher:   NewStatusWatcher(cfg.BaseURL, logFunc),
		logFunc:   logFunc,
		pauseCh:   make(chan struct{}, 1),
		restartCh: make(chan string, 1),
		state:     LoopStateIdle,
	}
}

// SetLogFunc sets a custom log function (for capturing output in UI).
func (l *Loop) SetLogFunc(f func(string)) {
	l.logFunc = f
	l.watcher.logFunc = f
}

// State returns the current loop state.
func (l *Loop) State() LoopState {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.state
}

func (l *Loop) setState(s LoopState) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = s
}

// Pause pauses the control loop. The loop will stop polling and dispatching
// until Resume is called.
func (l *Loop) Pause() {
	l.setState(LoopStatePaused)
	l.log("Loop paused by user")
}

// Resume resumes a paused control loop.
func (l *Loop) Resume() {
	if l.State() == LoopStatePaused {
		l.setState(LoopStateRunning)
		select {
		case l.pauseCh <- struct{}{}:
		default:
		}
		l.log("Loop resumed")
	}
}

// Restart restarts the loop from idle/done state with an optional new task prompt.
func (l *Loop) Restart(taskPrompt string) {
	select {
	case l.restartCh <- taskPrompt:
	default:
	}
}

func (l *Loop) log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	timestamp := time.Now().Format("15:04:05")
	l.logFunc(fmt.Sprintf("[%s] %s", timestamp, msg))
}

// Run starts the control loop. It blocks until the context is cancelled.
// The loop discovers agents, runs the orchestration cycle, and idles when done.
func (l *Loop) Run(ctx context.Context, initialPrompt string) error {
	l.log("Control loop starting")

	// Start the status watcher in the background
	go l.watcher.Run(ctx)

	prompt := initialPrompt

	// If no initial task, wait for one via restartCh (sent by __TASK__ stdin command)
	if prompt == "" {
		l.setState(LoopStateIdle)
		l.log("Waiting for task... Send one via the web UI input box.")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case prompt = <-l.restartCh:
			l.log("Task received")
		}
	}

	for {
		// Discover agents with retry (children may not exist yet)
		if err := l.discoverAgentsWithRetry(ctx); err != nil {
			l.log("Error discovering agents: %v", err)
			return fmt.Errorf("failed to discover agents: %w", err)
		}
		l.log("Discovered leader: %s, %d sub-agents: %v", l.leaderTitle, len(l.agentTitles), l.agentTitles)

		// Send initial prompt to orchestrator if provided
		if prompt != "" {
			teamDesc := l.buildTeamDescription()
			fullPrompt := teamDesc + "\n\n## Task\n\n" + prompt
			l.log("Sending task to orchestrator: %s", truncate(prompt, 100))
			if err := l.client.SendToInstance(l.leaderTitle, fullPrompt); err != nil {
				l.log("Error sending to orchestrator: %v", err)
				return fmt.Errorf("failed to send initial prompt: %w", err)
			}
		}

		// Run the main orchestration loop
		l.setState(LoopStateRunning)
		done, err := l.runLoop(ctx)
		if err != nil {
			return err
		}

		if done {
			// Orchestrator signaled done — idle and wait for restart
			l.setState(LoopStateDone)
			l.log("Orchestration complete. Waiting for user to restart...")

			select {
			case <-ctx.Done():
				l.log("Context cancelled, shutting down")
				return ctx.Err()
			case newPrompt := <-l.restartCh:
				l.log("Restart requested")
				prompt = newPrompt
				continue
			}
		} else {
			// Context was cancelled
			return ctx.Err()
		}
	}
}

// runLoop is the inner event-driven loop. Returns (done, error).
// done=true means the orchestrator signaled completion.
func (l *Loop) runLoop(ctx context.Context) (bool, error) {
	// Track which agents have become ready since the last dispatch
	pendingReady := make(map[string]bool)
	var batchTimer *time.Timer
	var batchCh <-chan time.Time // nil until batch window starts

	// Run heartbeat in a separate goroutine so it prints even when
	// the main loop is blocked waiting for the leader.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.printHeartbeat()
			}
		}
	}()

	changes := l.watcher.Changes()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()

		case change := <-changes:
			// Handle pause
			if l.State() == LoopStatePaused {
				l.log("Paused — waiting for resume...")
				select {
				case <-ctx.Done():
					return false, ctx.Err()
				case <-l.pauseCh:
					// Resumed
				}
			}

			// Update and check if this is a relevant agent transitioning to ready
			isAgent := false
			for _, t := range l.agentTitles {
				if t == change.Title {
					isAgent = true
					break
				}
			}
			if !isAgent {
				continue
			}

			l.log("Status change: %s → %s", change.Title, change.Status)

			if change.Status == "ready" && !pendingReady[change.Title] {
				pendingReady[change.Title] = true
				l.log("Agent ready: %s", change.Title)

				// Start batch window on first ready agent
				if batchTimer == nil {
					batchTimer = time.NewTimer(l.config.BatchWindow)
					batchCh = batchTimer.C
				}

				// If all agents are now ready, fire immediately
				allReady := true
				for _, t := range l.agentTitles {
					if !pendingReady[t] {
						s := l.watcher.GetStatus(t)
						if s != "ready" && s != "paused" {
							allReady = false
							break
						}
					}
				}
				if allReady {
					if batchTimer != nil {
						batchTimer.Stop()
					}
					batchTimer = nil
					batchCh = nil
					goto dispatch
				}
			}

		case <-batchCh:
			// Batch window expired
			batchTimer = nil
			batchCh = nil
			goto dispatch
		}
		continue

	dispatch:
		readyAgents := make([]string, 0, len(pendingReady))
		for t := range pendingReady {
			readyAgents = append(readyAgents, t)
		}
		pendingReady = make(map[string]bool)

		// Collect output from all newly-ready agents
		update := l.collectResults(readyAgents)

		// Send batched update to orchestrator
		promptText := update.FormatForPrompt()
		l.log("Sending batched update to orchestrator (%d agents)", len(update.Agents))
		if err := l.client.SendToInstance(l.leaderTitle, promptText); err != nil {
			l.log("Error sending to orchestrator: %v", err)
			continue
		}

		// Wait for orchestrator to finish processing
		l.log("Waiting for orchestrator to respond...")
		if err := l.waitForReady(ctx, l.leaderTitle); err != nil {
			l.log("Error waiting for orchestrator: %v", err)
			continue
		}

		// Read orchestrator's response
		response, err := l.readOrchestratorResponse()
		if err != nil {
			l.log("Could not parse structured response: %v", err)
			l.log("Orchestrator may have responded with natural language — check its session")
			continue
		}

		// Dispatch commands
		done := l.dispatchCommands(response)
		if done {
			return true, nil
		}
	}
}

// discoverAgentsWithRetry retries discovery every 2s for up to 60s,
// since child agents may not be created yet when the loop starts.
func (l *Loop) discoverAgentsWithRetry(ctx context.Context) error {
	deadline := time.After(60 * time.Second)
	for {
		err := l.discoverAgents()
		if err == nil {
			return nil
		}

		l.log("Waiting for agents to be created... (%v)", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for agents: %w", err)
		case <-time.After(2 * time.Second):
			// retry
		}
	}
}

// discoverAgents finds all child instances of the loop and identifies the leader.
func (l *Loop) discoverAgents() error {
	instances, err := l.client.ListInstances()
	if err != nil {
		return err
	}

	l.agentTitles = nil
	l.leaderTitle = ""
	for _, inst := range instances {
		if inst.Parent != l.groupTitle {
			continue
		}
		if inst.AgentPreset == "orchestrator" {
			l.leaderTitle = inst.Title
		} else {
			l.agentTitles = append(l.agentTitles, inst.Title)
		}
	}
	if l.leaderTitle == "" {
		return fmt.Errorf("no leader (orchestrator preset) found among children of %s", l.groupTitle)
	}
	return nil
}

// buildTeamDescription creates a description of the team for the orchestrator prompt.
func (l *Loop) buildTeamDescription() string {
	var b strings.Builder
	b.WriteString("## Your Team\n\n")
	b.WriteString("You are the orchestrator. You have the following agents available:\n\n")

	instances, err := l.client.ListInstances()
	if err != nil {
		b.WriteString("(Error loading agent details)\n")
		return b.String()
	}

	for _, inst := range instances {
		if inst.Parent == l.groupTitle && inst.Title != l.leaderTitle {
			preset := inst.AgentPreset
			if preset == "" {
				preset = "coder"
			}
			b.WriteString(fmt.Sprintf("- **%s** [%s] — working in `%s`\n", inst.Title, preset, inst.Path))
		}
	}
	return b.String()
}

// printHeartbeat prints current agent statuses to stdout.
func (l *Loop) printHeartbeat() {
	if len(l.agentTitles) == 0 {
		return
	}
	statuses := l.watcher.GetAll()
	parts := make([]string, 0, len(l.agentTitles)+2)
	parts = append(parts, fmt.Sprintf("[%s]", time.Now().Format("15:04:05")))
	for _, title := range l.agentTitles {
		s := statuses[title]
		if s == "" {
			s = "unknown"
		}
		parts = append(parts, fmt.Sprintf("%s:%s", title, s))
	}
	if l.leaderTitle != "" {
		s := statuses[l.leaderTitle]
		if s == "" {
			s = "unknown"
		}
		parts = append(parts, fmt.Sprintf("%s:%s", l.leaderTitle, s))
	}
	fmt.Println(strings.Join(parts, "  "))
}

// collectResults gathers output from newly-ready agents into a StatusUpdate.
func (l *Loop) collectResults(readyAgents []string) *StatusUpdate {
	update := &StatusUpdate{}
	for _, title := range readyAgents {
		output := ""
		history, err := l.client.GetInstanceHistory(title)
		if err != nil {
			l.log("Error getting history for %s: %v", title, err)
			output = "(error reading output)"
		} else {
			// Use the current pane content as the most recent output
			output = strings.Join(history.Pane, "\n")
			if output == "" && len(history.StableLines) > 0 {
				// Fall back to last N stable lines
				start := len(history.StableLines) - 50
				if start < 0 {
					start = 0
				}
				output = strings.Join(history.StableLines[start:], "\n")
			}
		}
		output = truncate(output, 4000)

		update.Agents = append(update.Agents, AgentStatus{
			Name:   title,
			Status: "ready",
			Output: output,
		})
	}
	return update
}

// waitForReady waits until the given instance status becomes "ready",
// checking the watcher's cached status on a short interval.
func (l *Loop) waitForReady(ctx context.Context, title string) error {
	if s := l.watcher.GetStatus(title); s == "ready" {
		return nil
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if s := l.watcher.GetStatus(title); s == "ready" {
				return nil
			}
		}
	}
}

// readOrchestratorResponse reads and parses the orchestrator's latest output.
func (l *Loop) readOrchestratorResponse() (*OrchestratorResponse, error) {
	history, err := l.client.GetInstanceHistory(l.leaderTitle)
	if err != nil {
		return nil, fmt.Errorf("failed to read orchestrator history: %w", err)
	}

	// The pane content contains the most recent output
	text := strings.Join(history.Pane, "\n")
	if text == "" && len(history.StableLines) > 0 {
		// Fall back to recent stable lines
		start := len(history.StableLines) - 100
		if start < 0 {
			start = 0
		}
		text = strings.Join(history.StableLines[start:], "\n")
	}

	return ParseOrchestratorResponse(text)
}

// dispatchCommands executes the orchestrator's commands. Returns true if "done".
func (l *Loop) dispatchCommands(resp *OrchestratorResponse) bool {
	for _, cmd := range resp.Commands {
		switch cmd.Action {
		case "dispatch":
			l.log("Dispatching to %s: %s", cmd.Agent, truncate(cmd.Prompt, 80))
			if err := l.client.SendToInstance(cmd.Agent, cmd.Prompt); err != nil {
				l.log("Error dispatching to %s: %v", cmd.Agent, err)
			}
		case "share_context":
			l.log("Sharing context from %s to %s", cmd.From, cmd.To)
			contextMsg := fmt.Sprintf("Context from %s:\n\n%s", cmd.From, cmd.Context)
			if err := l.client.SendToInstance(cmd.To, contextMsg); err != nil {
				l.log("Error sharing context to %s: %v", cmd.To, err)
			}
		case "done":
			l.log("Orchestrator signaled DONE: %s", cmd.Summary)
			return true
		case "wait":
			l.log("Orchestrator is waiting for agents to complete")
		default:
			l.log("Unknown command action: %s", cmd.Action)
		}
	}
	return false
}

// truncate truncates a string to maxLen characters, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
