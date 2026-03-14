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

// StatusUpdate represents a status change for one agent.
type StatusChange struct {
	Title  string
	Status string
}

// StatusWatcher connects to the web server's status websocket and
// pushes status changes to a channel.
type StatusWatcher struct {
	baseURL  string
	statuses map[string]string // current known statuses
	mu       sync.RWMutex
	ch       chan StatusChange
	logFunc  func(string)
}

// NewStatusWatcher creates a watcher that will connect to the given base URL.
func NewStatusWatcher(baseURL string, logFunc func(string)) *StatusWatcher {
	return &StatusWatcher{
		baseURL:  baseURL,
		statuses: make(map[string]string),
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
			Type     string            `json:"type"`
			Statuses map[string]string `json:"statuses"`
		}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		if msg.Type != "status_update" || msg.Statuses == nil {
			continue
		}

		w.mu.Lock()
		for title, status := range msg.Statuses {
			prev := w.statuses[title]
			w.statuses[title] = status
			if prev != status {
				select {
				case w.ch <- StatusChange{Title: title, Status: status}:
				default:
					// Channel full, drop oldest
				}
			}
		}
		w.mu.Unlock()
	}
}
