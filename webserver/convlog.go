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
// GetState deduplicates at read time: if the tail of stableLines overlaps with
// the head of currentPane (which can happen after a terminal resize pulls lines
// back from scrollback into the visible pane), the overlapping stable lines are
// excluded from the returned history.
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

// slicesEqual reports whether two string slices have identical contents.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// findOverlap returns the length of the longest suffix of stable that matches
// a prefix of pane. This handles the case where a terminal resize causes lines
// to return from scrollback into the visible pane (they appear at both the tail
// of stable and the head of pane). Bounded by pane size (~50 lines).
func findOverlap(stable, pane []string) int {
	maxOverlap := len(pane)
	if maxOverlap > len(stable) {
		maxOverlap = len(stable)
	}
	for overlap := maxOverlap; overlap > 0; overlap-- {
		if slicesEqual(stable[len(stable)-overlap:], pane[:overlap]) {
			return overlap
		}
	}
	return 0
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
// Stable lines are trimmed to exclude any trailing overlap with the pane head.
// This handles the resize case where lines return from scrollback to the visible pane.
func (cl *ConversationLog) GetState() (stableLines []string, stableSeqNo uint64, pane []string, lastInput string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	paneOut := make([]string, len(cl.currentPane))
	copy(paneOut, cl.currentPane)

	overlap := findOverlap(cl.stableLines, cl.currentPane)
	trimmedLen := len(cl.stableLines) - overlap

	stable := make([]string, trimmedLen)
	copy(stable, cl.stableLines[:trimmedLen])

	return stable, cl.stableSeqNo, paneOut, cl.lastInput
}

// GetRawStableCount returns the internal (untrimmed) stable line count.
// Used by the WS handler to track position without overlap fluctuations.
func (cl *ConversationLog) GetRawStableCount() int {
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
