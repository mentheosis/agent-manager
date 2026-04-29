package main

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
// It monitors agent status changes and feeds batched results to the orchestrator session.
type Loop struct {
	config Config
	client *Client

	// groupTitle is the title of the loop instance itself (the parent of all children).
	groupTitle string
	// leaderTitle is the title of the leader/orchestrator Claude session.
	leaderTitle string
	// agentTitles are the titles of the sub-agent sessions (excluding the leader).
	agentTitles []string

	// state tracks the loop's current state.
	state LoopState
	mu    sync.RWMutex

	// watcher monitors agent statuses.
	watcher *StatusWatcher

	// logFunc is called for each log line. Defaults to fmt.Println.
	logFunc func(string)

	// pauseCh is used to signal pause/resume.
	pauseCh chan struct{}
	// restartCh is used to signal a restart from idle/done state.
	restartCh chan string // carries optional new task prompt
	// doneCh receives a summary when the leader calls mark_task_done via MCP.
	doneCh <-chan string
	// taskCh receives tasks from the MCP HTTP /task endpoint.
	taskCh <-chan string
}

// NewLoop creates a new control loop. groupTitle is the title of the loop instance.
func NewLoop(cfg Config, groupTitle string) *Loop {
	logFunc := func(s string) { fmt.Println(s) }
	client := NewClient(cfg.BaseURL)
	return &Loop{
		config:     cfg,
		client:     client,
		groupTitle: groupTitle,
		watcher:    NewStatusWatcher(client, groupTitle, cfg.PollInterval, logFunc),
		logFunc:    logFunc,
		pauseCh:    make(chan struct{}, 1),
		restartCh:  make(chan string, 1),
		state:      LoopStateIdle,
	}
}

// SetLogFunc sets a custom log function.
func (l *Loop) SetLogFunc(f func(string)) {
	l.logFunc = f
	l.watcher.logFunc = f
}

// SetDoneCh sets the channel that signals task completion from the MCP server.
func (l *Loop) SetDoneCh(ch <-chan string) {
	l.doneCh = ch
}

// SetTaskCh sets the channel for receiving tasks from the MCP HTTP /task endpoint.
func (l *Loop) SetTaskCh(ch <-chan string) {
	l.taskCh = ch
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

// Pause pauses the control loop.
func (l *Loop) Pause() {
	l.setState(LoopStatePaused)
	l.log("Loop paused by user")
}

// Resume resumes a paused or done control loop.
func (l *Loop) Resume() {
	state := l.State()
	switch state {
	case LoopStatePaused:
		l.setState(LoopStateRunning)
		select {
		case l.pauseCh <- struct{}{}:
		default:
		}
		l.log("Loop resumed from paused")
	case LoopStateDone, LoopStateIdle:
		l.log("Loop resumed from %s", state)
		select {
		case l.restartCh <- "":
		default:
		}
	}
}

// Restart restarts the loop with an optional new task prompt.
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
func (l *Loop) Run(ctx context.Context, initialPrompt string) error {
	l.log("Control loop starting")

	// Start the status watcher in the background
	go l.watcher.Run(ctx)

	prompt := initialPrompt

	// If no initial task, wait for one
	if prompt == "" {
		l.setState(LoopStateIdle)
		l.log("Waiting for task...")
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case prompt = <-l.restartCh:
				l.log("Task received (restart)")
			case prompt = <-l.taskCh:
				l.log("Task received (HTTP)")
			}
			break
		}
	}

	for {
		// Discover agents
		if err := l.discoverAgentsWithRetry(ctx); err != nil {
			l.log("Error discovering agents: %v", err)
			return fmt.Errorf("failed to discover agents: %w", err)
		}
		l.log("Discovered leader: %s, %d sub-agents: %v", l.leaderTitle, len(l.agentTitles), l.agentTitles)

		// Send initial prompt to orchestrator
		if prompt != "" {
			teamDesc := l.buildTeamDescription()
			fullPrompt := teamDesc + "\n\n## Task\n\n" + prompt
			l.log("Sending initial task to orchestrator...")
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
			l.log("Orchestration complete. Waiting for restart...")

			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case newPrompt := <-l.restartCh:
					prompt = newPrompt
				case newPrompt := <-l.taskCh:
					prompt = newPrompt
				}
				break
			}
			continue
		} else {
			return ctx.Err()
		}
	}
}

// runLoop is the inner event-driven loop. Returns (done, error).
func (l *Loop) runLoop(ctx context.Context) (bool, error) {
	lastStatus := make(map[string]string)

	// Channel for signaling all agents are idle
	allIdleCh := make(chan struct{}, 1)
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)

	// Heartbeat goroutine
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		consecutiveIdleCount := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				l.printHeartbeat()
				// Check if all agents AND the leader are idle
				if len(l.agentTitles) > 0 {
					allIdle := true
					for _, t := range l.agentTitles {
						s := l.watcher.GetStatus(t)
						if s != "ready" && s != "" {
							allIdle = false
							break
						}
					}
					// Check leader too
					if allIdle && l.leaderTitle != "" {
						ls := l.watcher.GetStatus(l.leaderTitle)
						if ls != "ready" && ls != "" {
							allIdle = false
						}
					}
					if allIdle {
						consecutiveIdleCount++
						if consecutiveIdleCount >= 2 && consecutiveIdleCount%2 == 0 {
							select {
							case allIdleCh <- struct{}{}:
							default:
							}
						}
					} else {
						consecutiveIdleCount = 0
					}
				}
			}
		}
	}()

	// Error check goroutine - periodically check for permission errors
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopHeartbeat:
				return
			case <-ticker.C:
				errored := l.watcher.CheckForErrors()
				if len(errored) > 0 {
					l.notifyLeaderOfErrors(ctx, errored)
				}
			}
		}
	}()

	var doneCh <-chan string
	if l.doneCh != nil {
		doneCh = l.doneCh
	}

	changes := l.watcher.Changes()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()

		case summary := <-doneCh:
			l.log("Task completed (via MCP): %s", summary)
			l.Pause()
			return true, nil

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

			// Only track agents in our group
			isAgent := false
			for _, t := range l.agentTitles {
				if t == change.Title {
					isAgent = true
					break
				}
			}
			if !isAgent && change.Title != l.leaderTitle {
				continue
			}

			if change.Status != lastStatus[change.Title] {
				lastStatus[change.Title] = change.Status
				l.log("Status change: %s → %s", change.Title, change.Status)
			}

		case <-allIdleCh:
			// All agents AND leader are idle — nudge the leader
			leaderStatus := l.watcher.GetStatus(l.leaderTitle)
			if leaderStatus != "ready" {
				l.log("All agents idle but leader is %s — skipping", leaderStatus)
				continue
			}

			update := &StatusUpdate{Message: "All agents are idle. Please review and take action."}
			for _, t := range l.agentTitles {
				s := l.watcher.GetStatus(t)
				update.Agents = append(update.Agents, AgentStatus{Name: t, Status: s})
			}

			promptText := update.FormatForPrompt()
			l.log("All idle — nudging orchestrator")

			if err := l.client.SendToInstance(l.leaderTitle, promptText); err != nil {
				l.log("Error sending to orchestrator: %v", err)
			}
		}
	}
}

// notifyLeaderOfErrors sends error information to the leader.
func (l *Loop) notifyLeaderOfErrors(ctx context.Context, errored []AgentStatus) {
	leaderStatus := l.watcher.GetStatus(l.leaderTitle)
	if leaderStatus != "ready" {
		return
	}

	var b strings.Builder
	b.WriteString("## Agent Errors Detected\n\n")
	b.WriteString("The following agents have encountered errors:\n\n")
	for _, a := range errored {
		b.WriteString(fmt.Sprintf("- **%s**: %s\n", a.Name, a.ErrorSummary))
	}
	b.WriteString("\nPlease check with read_agent_output and decide how to proceed.\n")

	l.log("Notifying leader of %d agent errors", len(errored))
	if err := l.client.SendToInstance(l.leaderTitle, b.String()); err != nil {
		l.log("Error notifying leader: %v", err)
	}
}

func (l *Loop) discoverAgentsWithRetry(ctx context.Context) error {
	deadline := time.After(60 * time.Second)
	for {
		err := l.discoverAgents()
		if err == nil {
			return nil
		}

		l.log("Waiting for agents... (%v)", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timed out waiting for agents: %w", err)
		case <-time.After(2 * time.Second):
		}
	}
}

func (l *Loop) discoverAgents() error {
	children, err := l.client.GetChildren(l.groupTitle)
	if err != nil {
		return err
	}

	l.agentTitles = nil
	l.leaderTitle = ""
	for _, inst := range children {
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

func (l *Loop) buildTeamDescription() string {
	var b strings.Builder
	b.WriteString("## Your Team\n\n")
	b.WriteString("You are the orchestrator. You have the following agents available:\n\n")

	children, err := l.client.GetChildren(l.groupTitle)
	if err != nil {
		b.WriteString("(Error loading team info)\n")
		return b.String()
	}

	for _, inst := range children {
		if inst.AgentPreset == "orchestrator" {
			continue
		}
		preset := inst.AgentPreset
		if preset == "" {
			preset = "coder"
		}
		b.WriteString(fmt.Sprintf("- **%s** (%s): working in `%s`\n", inst.Title, preset, inst.Path))
	}

	b.WriteString("\nUse your MCP tools to coordinate them.\n")
	return b.String()
}

func (l *Loop) printHeartbeat() {
	var parts []string
	for _, t := range l.agentTitles {
		s := l.watcher.GetStatus(t)
		if s == "" {
			s = "?"
		}
		parts = append(parts, fmt.Sprintf("%s:%s", t, s))
	}
	if l.leaderTitle != "" {
		ls := l.watcher.GetStatus(l.leaderTitle)
		if ls == "" {
			ls = "?"
		}
		parts = append(parts, fmt.Sprintf("leader:%s", ls))
	}
	l.log("Heartbeat: %s", strings.Join(parts, " "))
}
