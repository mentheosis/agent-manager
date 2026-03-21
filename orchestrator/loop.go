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
	// doneCh receives a summary when the leader calls mark_task_done via MCP.
	doneCh <-chan string
	// taskCh receives tasks from the MCP HTTP /task endpoint.
	taskCh <-chan string
	// rediscoverCh signals the loop to re-discover its agents mid-run.
	rediscoverCh chan struct{}
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
		pauseCh:      make(chan struct{}, 1),
		restartCh:    make(chan string, 1),
		rediscoverCh: make(chan struct{}, 1),
		state:        LoopStateIdle,
	}
}

// SetLogFunc sets a custom log function (for capturing output in UI).
func (l *Loop) SetLogFunc(f func(string)) {
	l.logFunc = f
	l.watcher.logFunc = f
}

// SetDoneCh sets the channel that signals task completion from the MCP server.
func (l *Loop) SetDoneCh(ch <-chan string) {
	l.doneCh = ch
}

// SetTaskCh sets an additional channel for receiving tasks (from the MCP HTTP /task endpoint).
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

// Pause pauses the control loop. The loop will stop polling and dispatching
// until Resume is called.
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
		// Restart the loop with an empty prompt (task already sent to leader)
		l.log("Loop resumed from %s", state)
		select {
		case l.restartCh <- "":
		default:
		}
	}
}

// Restart restarts the loop from idle/done state with an optional new task prompt.
func (l *Loop) Restart(taskPrompt string) {
	select {
	case l.restartCh <- taskPrompt:
	default:
	}
}

// handleRediscover performs agent re-discovery and logs joins/leaves.
func (l *Loop) handleRediscover() {
	oldAgents := make(map[string]bool, len(l.agentTitles))
	for _, t := range l.agentTitles {
		oldAgents[t] = true
	}
	l.log("Rediscovering agents...")
	if err := l.discoverAgents(); err != nil {
		l.log("Rediscovery failed: %v", err)
		return
	}
	// Report joins
	for _, t := range l.agentTitles {
		if !oldAgents[t] {
			l.log("%s %s", colorize(ansiBold+ansiGreen, "▶ Agent joined:"), t)
		}
	}
	// Report leaves
	newAgents := make(map[string]bool, len(l.agentTitles))
	for _, t := range l.agentTitles {
		newAgents[t] = true
	}
	for t := range oldAgents {
		if !newAgents[t] {
			l.log("%s %s", colorize(ansiBold+ansiYellow, "◀ Agent left:"), t)
		}
	}
	l.log("%s: %d agents=%v", colorize(ansiGreen, "Team updated"), len(l.agentTitles), l.agentTitles)
}

// Rediscover signals the loop to re-discover its agents (e.g. after reparenting).
func (l *Loop) Rediscover() {
	select {
	case l.rediscoverCh <- struct{}{}:
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

	// If no initial task, wait for one via restartCh or taskCh
	if prompt == "" {
		l.setState(LoopStateIdle)
		l.log("Waiting for task... Send one via the web UI.")
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case prompt = <-l.restartCh:
				l.log("Task received (stdin)")
			case prompt = <-l.taskCh:
				l.log("Task received (HTTP)")
			case <-l.rediscoverCh:
				l.handleRediscover()
				continue
			}
			break
		}
	}

	for {
		// Discover agents with retry (children may not exist yet)
		if err := l.discoverAgentsWithRetry(ctx); err != nil {
			l.log("Error discovering agents: %v", err)
			return fmt.Errorf("failed to discover agents: %w", err)
		}
		l.log("%s: %s, %d sub-agents: %v", colorize(ansiGreen, "Discovered leader"), l.leaderTitle, len(l.agentTitles), l.agentTitles)

		// Send initial prompt to orchestrator if provided
		if prompt != "" {
			teamDesc := l.buildTeamDescription()
			fullPrompt := teamDesc + "\n\n## Task\n\n" + prompt
			l.log("%s\n%s", colorize(ansiBold+ansiCyan, "▶ Sending initial task to orchestrator"), fullPrompt)
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
			l.log(colorize(ansiBold+ansiGreen, "✓ Orchestration complete.") + " Waiting for user to restart...")

			for {
				select {
				case <-ctx.Done():
					l.log("Context cancelled, shutting down")
					return ctx.Err()
				case newPrompt := <-l.restartCh:
					l.log("Restart requested (stdin)")
					prompt = newPrompt
				case newPrompt := <-l.taskCh:
					l.log("Restart requested (HTTP)")
					prompt = newPrompt
				case <-l.rediscoverCh:
					l.handleRediscover()
					continue
				}
				break
			}
			continue
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
	lastStatus := make(map[string]string) // suppress duplicate status change logs
	var batchTimer *time.Timer
	var batchCh <-chan time.Time // nil until batch window starts

	// Channel for the heartbeat goroutine to signal all agents are idle
	allIdleCh := make(chan struct{}, 1)
	// stopHeartbeat is closed when runLoop returns, so the heartbeat goroutine exits.
	stopHeartbeat := make(chan struct{})
	defer close(stopHeartbeat)

	// Run heartbeat in a separate goroutine so it prints even when
	// the main loop is blocked waiting for the leader.
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
					// Check sub-agents
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
						// Only notify after 2 consecutive idle heartbeats, and only once
						if consecutiveIdleCount == 2 {
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

	// doneCh from the MCP server (may be nil if no MCP server)
	var doneCh <-chan string
	if l.doneCh != nil {
		doneCh = l.doneCh
	}

	changes := l.watcher.Changes()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()

		case <-l.rediscoverCh:
			l.handleRediscover()

		case summary := <-doneCh:
			// Leader called mark_task_done via MCP
			l.log("%s: %s", colorize(ansiBold+ansiGreen, "✓ Task completed (via MCP)"), summary)
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

			if change.Status == lastStatus[change.Title] {
				continue
			}
			lastStatus[change.Title] = change.Status
			l.log("Status change: %s → %s", change.Title, colorize(ansiYellow, change.Status))

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

		case <-allIdleCh:
			// Heartbeat detected all agents idle — only notify if leader is also idle.
			// If the leader is busy, it's already working on dispatching tasks.
			leaderStatus := l.watcher.GetStatus(l.leaderTitle)
			if leaderStatus != "ready" {
				l.log("All agents idle but leader is %s — skipping notification", colorize(ansiYellow, leaderStatus))
				continue
			}
			idleMsg := "All agents are currently idle (status: ready/waiting). " +
				"If the task is complete, call the mark_task_done tool with a summary. " +
				"If more work is needed, use send_to_agent to dispatch the next steps."
			l.log("%s\n%s", colorize(ansiBold+ansiYellow, "⚠ All agents idle — notifying orchestrator"), colorize(ansiDim, idleMsg))
			if err := l.client.SendToInstance(l.leaderTitle, idleMsg); err != nil {
				l.log("Error sending idle notification: %v", err)
			}
		}
		continue

	dispatch:
		pendingReady = make(map[string]bool)

		// Wait for the leader to be idle before sending — avoids queueing
		// redundant messages while the leader is still processing.
		leaderStatus := l.watcher.GetStatus(l.leaderTitle)
		if leaderStatus != "ready" && leaderStatus != "" {
			l.log("Leader busy (%s), waiting for idle before dispatching...", colorize(ansiYellow, leaderStatus))
			if err := l.waitForReady(ctx, l.leaderTitle); err != nil {
				return false, err
			}
			l.log("Leader now idle, dispatching")
		}

		// Collect output from ALL agents that are currently ready (not just
		// the ones that triggered the dispatch). This naturally batches results
		// from agents that became ready while we waited for the leader.
		var readyAgents []string
		for _, t := range l.agentTitles {
			s := l.watcher.GetStatus(t)
			if s == "ready" {
				readyAgents = append(readyAgents, t)
			}
		}

		if len(readyAgents) == 0 {
			continue // all agents went back to running while we waited
		}

		update := l.collectResults(readyAgents)

		// Append prompt info for agents that have interactive prompts
		for i, agent := range update.Agents {
			meta := l.watcher.GetAgentMeta(agent.Name)
			if meta != nil && meta.Prompt != nil && len(meta.Prompt.Actions) > 0 {
				update.Agents[i].Prompt = meta.Prompt
			}
		}

		// Send batched status update to orchestrator — the leader will use
		// MCP tools (send_to_agent, read_agent_output, mark_task_done) to act on it.
		promptText := update.FormatForPrompt()
		displayText := update.FormatForDisplay()
		l.log("%s\n%s", colorize(ansiBold+ansiCyan, fmt.Sprintf("▶ Sending batched update to orchestrator (%d agents)", len(update.Agents))), displayText)
		if err := l.client.SendToInstance(l.leaderTitle, promptText); err != nil {
			l.log("Error sending to orchestrator: %v", err)
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
	allMeta := l.watcher.GetAllAgentMeta()
	parts := make([]string, 0, len(l.agentTitles)+2)
	parts = append(parts, fmt.Sprintf("[%s]", time.Now().Format("15:04:05")))
	for _, title := range l.agentTitles {
		meta := allMeta[title]
		if meta == nil {
			parts = append(parts, fmt.Sprintf("%s:unknown", title))
		} else if meta.Prompt != nil && len(meta.Prompt.Actions) > 0 {
			parts = append(parts, colorize(ansiYellow, fmt.Sprintf("%s:%s(prompt)", title, meta.Status)))
		} else {
			statusColor := ansiBrightBlk
			if meta.Status == "running" {
				statusColor = ansiGreen
			} else if meta.Status == "ready" {
				statusColor = ansiYellow
			}
			parts = append(parts, fmt.Sprintf("%s:%s", title, colorize(statusColor, meta.Status)))
		}
	}
	if l.leaderTitle != "" {
		meta := allMeta[l.leaderTitle]
		if meta == nil {
			parts = append(parts, fmt.Sprintf("%s:unknown", l.leaderTitle))
		} else {
			parts = append(parts, fmt.Sprintf("%s:%s", l.leaderTitle, meta.Status))
		}
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


// truncate truncates a string to maxLen characters, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
