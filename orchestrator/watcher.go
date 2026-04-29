package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// StatusChange represents a status change for one agent.
type StatusChange struct {
	Title    string
	Status   string
	HasError bool
}

// StatusWatcher monitors the status of child agents in a loop instance.
// It polls the agent-manager API periodically and emits changes.
type StatusWatcher struct {
	client       *Client
	groupTitle   string
	pollInterval time.Duration
	statuses     map[string]string // current known statuses
	mu           sync.RWMutex
	ch           chan StatusChange
	logFunc      func(string)
}

// NewStatusWatcher creates a watcher for the given loop instance.
func NewStatusWatcher(client *Client, groupTitle string, pollInterval time.Duration, logFunc func(string)) *StatusWatcher {
	return &StatusWatcher{
		client:       client,
		groupTitle:   groupTitle,
		pollInterval: pollInterval,
		statuses:     make(map[string]string),
		ch:           make(chan StatusChange, 64),
		logFunc:      logFunc,
	}
}

// Changes returns the channel that receives status changes.
func (w *StatusWatcher) Changes() <-chan StatusChange {
	return w.ch
}

// GetStatus returns the last known status for a title.
func (w *StatusWatcher) GetStatus(title string) string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.statuses[title]
}

// GetAll returns a copy of all known statuses.
func (w *StatusWatcher) GetAll() map[string]string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	cp := make(map[string]string, len(w.statuses))
	for k, v := range w.statuses {
		cp[k] = v
	}
	return cp
}

func (w *StatusWatcher) log(format string, args ...interface{}) {
	if w.logFunc != nil {
		w.logFunc(fmt.Sprintf("[Watcher] "+format, args...))
	}
}

// Run polls the agent-manager API periodically and emits status changes.
// It runs until ctx is cancelled.
func (w *StatusWatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// Initial poll
	w.poll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll()
		}
	}
}

func (w *StatusWatcher) poll() {
	children, err := w.client.GetChildren(w.groupTitle)
	if err != nil {
		w.log("Error polling children: %v", err)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Track which agents we've seen this poll
	seen := make(map[string]bool)

	for _, inst := range children {
		seen[inst.Title] = true
		prev := w.statuses[inst.Title]
		w.statuses[inst.Title] = inst.Status

		if prev != inst.Status {
			select {
			case w.ch <- StatusChange{Title: inst.Title, Status: inst.Status}:
			default:
				// Channel full, skip
			}
		}
	}

	// Remove agents that are no longer children
	for title := range w.statuses {
		if !seen[title] {
			delete(w.statuses, title)
		}
	}
}

// CheckForErrors checks recent events for any agents that have error tool results.
// Returns a list of agents with errors and their most recent error.
func (w *StatusWatcher) CheckForErrors() []AgentStatus {
	var errored []AgentStatus

	w.mu.RLock()
	titles := make([]string, 0, len(w.statuses))
	for t := range w.statuses {
		titles = append(titles, t)
	}
	w.mu.RUnlock()

	for _, title := range titles {
		// Check last 10 events for errors
		history, err := w.client.GetInstanceHistory(title, 10, 0, "tool_result")
		if err != nil {
			continue
		}

		for _, event := range history.Events {
			if event.IsError {
				errored = append(errored, AgentStatus{
					Name:         title,
					Status:       w.GetStatus(title),
					HasError:     true,
					ErrorSummary: truncate(event.Output, 100),
				})
				break // Only report most recent error per agent
			}
		}
	}

	return errored
}
