package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WatcherPaneAction mirrors webserver.PaneAction for the orchestrator side.
type WatcherPaneAction struct {
	Type  string `json:"type"`  // "option" or "shortcut"
	Label string `json:"label"`
	Key   string `json:"key"`
}

// WatcherPanePrompt mirrors webserver.PanePrompt.
type WatcherPanePrompt struct {
	Message string              `json:"message"`
	Actions []WatcherPaneAction `json:"actions"`
}

// WatcherAgentMeta mirrors webserver.AgentMeta — enriched status with prompt info.
type WatcherAgentMeta struct {
	Status string             `json:"status"`
	Prompt *WatcherPanePrompt `json:"prompt,omitempty"`
}

// StatusChange represents a status change for one agent.
type StatusChange struct {
	Title  string
	Status string
	Prompt *WatcherPanePrompt // non-nil when agent has an interactive prompt
}

// StatusWatcher connects to the web server's status websocket and
// pushes status changes to a channel.
type StatusWatcher struct {
	baseURL  string
	statuses map[string]string            // current known statuses
	agents   map[string]*WatcherAgentMeta // enriched metadata
	mu       sync.RWMutex
	ch       chan StatusChange
	logFunc  func(string)
}

// NewStatusWatcher creates a watcher that will connect to the given base URL.
func NewStatusWatcher(baseURL string, logFunc func(string)) *StatusWatcher {
	return &StatusWatcher{
		baseURL:  baseURL,
		statuses: make(map[string]string),
		agents:   make(map[string]*WatcherAgentMeta),
		ch:       make(chan StatusChange, 64),
		logFunc:  logFunc,
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

// GetAgentMeta returns the enriched metadata for a specific agent, or nil if unknown.
func (w *StatusWatcher) GetAgentMeta(title string) *WatcherAgentMeta {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.agents[title]
}

// GetAllAgentMeta returns a copy of all known agent metadata.
func (w *StatusWatcher) GetAllAgentMeta() map[string]*WatcherAgentMeta {
	w.mu.RLock()
	defer w.mu.RUnlock()
	cp := make(map[string]*WatcherAgentMeta, len(w.agents))
	for k, v := range w.agents {
		cp[k] = v
	}
	return cp
}

func (w *StatusWatcher) log(format string, args ...interface{}) {
	if w.logFunc != nil {
		w.logFunc(fmt.Sprintf(format, args...))
	}
}

// Run connects to the websocket and reads status updates until ctx is cancelled.
// It reconnects automatically on disconnect.
func (w *StatusWatcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := w.connect(ctx)
		if err != nil && ctx.Err() == nil {
			w.log("Status websocket disconnected: %v — reconnecting in 2s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (w *StatusWatcher) connect(ctx context.Context) error {
	// Convert http:// to ws://
	wsURL := strings.Replace(w.baseURL, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL += "/api/statuses/ws"

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	w.log("Connected to status websocket at %s", wsURL)

	// Set up a close handler triggered by context cancellation
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}

		var msg struct {
			Type     string                       `json:"type"`
			Statuses map[string]string             `json:"statuses"`
			Agents   map[string]*WatcherAgentMeta `json:"agents"`
		}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg.Type != "status_update" {
			continue
		}

		w.mu.Lock()

		// Use enriched agents data if available, fall back to plain statuses
		if msg.Agents != nil {
			for title, meta := range msg.Agents {
				prev := w.statuses[title]
				prevMeta := w.agents[title]
				w.statuses[title] = meta.Status
				w.agents[title] = meta

				// Emit change if status changed or prompt changed
				statusChanged := prev != meta.Status
				promptChanged := !promptEqual(prevMeta, meta)
				if statusChanged || promptChanged {
					var prompt *WatcherPanePrompt
					if meta.Prompt != nil {
						prompt = meta.Prompt
					}
					select {
					case w.ch <- StatusChange{Title: title, Status: meta.Status, Prompt: prompt}:
					default:
					}
				}
			}
		} else if msg.Statuses != nil {
			// Backwards-compatible: plain statuses only
			for title, status := range msg.Statuses {
				prev := w.statuses[title]
				w.statuses[title] = status
				if prev != status {
					select {
					case w.ch <- StatusChange{Title: title, Status: status}:
					default:
					}
				}
			}
		}

		w.mu.Unlock()
	}
}

// promptEqual compares the prompt portion of two AgentMeta values.
func promptEqual(a, b *WatcherAgentMeta) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	pa, pb := a.Prompt, b.Prompt
	if pa == nil && pb == nil {
		return true
	}
	if pa == nil || pb == nil {
		return false
	}
	if pa.Message != pb.Message {
		return false
	}
	if len(pa.Actions) != len(pb.Actions) {
		return false
	}
	for i := range pa.Actions {
		if pa.Actions[i] != pb.Actions[i] {
			return false
		}
	}
	return true
}
