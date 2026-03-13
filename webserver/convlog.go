package webserver

import (
	"strings"
	"sync"
)

// ConversationLog accumulates terminal output by separating tmux scrollback
// (stable, append-only) from the visible pane (volatile, constantly rewritten).
//
// Stable lines grow only via scrollback delta: the caller detects new scrollback
// lines (via tmux history_size metadata) and passes only the delta to Ingest,
// which appends them to stableLines. There is no turn-based promotion — lines
// that never scroll off the visible pane remain in currentPane and are always
// visible to the client via pane messages.
//
// Since scrollback lines have, by definition, already left the visible pane
// before they enter stableLines, there is no overlap between stable and pane
// and no deduplication is needed.
type ConversationLog struct {
	mu           sync.Mutex
	stableLines  []string // finalized lines (append-only, from scrollback delta only)
	currentPane  []string // volatile visible pane (replaced each ingest)
	inputHistory []string // prompts sent by user
	stableSeqNo  uint64   // bumped when stableLines grows
	lastStatus   string   // tracks instance status
	lastInput    string   // most recent user prompt
}

// NewConversationLog creates a new empty ConversationLog.
func NewConversationLog() *ConversationLog {
	return &ConversationLog{}
}

// splitLines splits text into lines and strips trailing blank lines.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Ingest processes new terminal output:
//   - newScrollback: lines that have newly scrolled off the visible pane since
//     the last call (may be empty when nothing has scrolled)
//   - paneCapture: the current visible pane content
func (cl *ConversationLog) Ingest(newScrollback string, paneCapture string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	newLines := splitLines(newScrollback)
	paneLines := splitLines(paneCapture)

	if len(newLines) > 0 {
		cl.stableLines = append(cl.stableLines, newLines...)
		cl.stableSeqNo++
	}

	cl.currentPane = paneLines
}

// SetStatus updates the instance status. Status is tracked for informational
// purposes but does not affect stable line promotion.
func (cl *ConversationLog) SetStatus(status string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.lastStatus = status
}

// SetLastInput records the most recent user prompt.
func (cl *ConversationLog) SetLastInput(text string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.lastInput = text
}

// GetLastInput returns the most recent user prompt.
func (cl *ConversationLog) GetLastInput() string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.lastInput
}

// GetState returns the current stable lines, sequence number, volatile pane, and last input.
// Stable lines are returned directly — no deduplication is needed because
// scrollback lines have already left the visible pane before entering stable.
func (cl *ConversationLog) GetState() (stableLines []string, stableSeqNo uint64, pane []string, lastInput string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	paneOut := make([]string, len(cl.currentPane))
	copy(paneOut, cl.currentPane)

	stable := make([]string, len(cl.stableLines))
	copy(stable, cl.stableLines)

	return stable, cl.stableSeqNo, paneOut, cl.lastInput
}

// TrimStable removes the last n lines from stableLines. This is used when
// tmux history_size decreases (e.g. pane height increase or terminal resize),
// meaning lines have returned from scrollback into the visible pane.
func (cl *ConversationLog) TrimStable(n int) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if n <= 0 {
		return
	}
	if n >= len(cl.stableLines) {
		cl.stableLines = nil
	} else {
		cl.stableLines = cl.stableLines[:len(cl.stableLines)-n]
	}
	cl.stableSeqNo++
}

// GetStableCount returns the number of stable lines.
func (cl *ConversationLog) GetStableCount() int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return len(cl.stableLines)
}

// GetStableSince returns stable lines starting from the given index (for delta fetching).
func (cl *ConversationLog) GetStableSince(fromIndex int) []string {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	if fromIndex >= len(cl.stableLines) {
		return nil
	}
	if fromIndex < 0 {
		fromIndex = 0
	}
	result := make([]string, len(cl.stableLines)-fromIndex)
	copy(result, cl.stableLines[fromIndex:])
	return result
}

// AddInput records a sent prompt in the input history.
func (cl *ConversationLog) AddInput(text string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.inputHistory = append(cl.inputHistory, text)
}

// GetInputHistory returns all recorded input prompts.
func (cl *ConversationLog) GetInputHistory() []string {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	result := make([]string, len(cl.inputHistory))
	copy(result, cl.inputHistory)
	return result
}
