package webserver

import (
	"strings"
	"sync"
)

// ConversationLog accumulates terminal output by separating tmux scrollback
// (stable, append-only) from the visible pane (volatile, constantly rewritten).
//
// Two mechanisms preserve output:
//  1. Scrollback promotion: the caller detects new scrollback lines (via tmux
//     history_size metadata) and passes only the delta to Ingest, which appends
//     them to stableLines.
//  2. Turn-based promotion: when the instance status transitions to "ready",
//     all current pane content is promoted to stable history so that responses
//     which never scroll off the visible pane are not lost.
//
// GetState deduplicates at read time: if the tail of stableLines overlaps with
// the head of currentPane, the overlapping stable lines are excluded from the
// returned history. This ensures the client always receives the real pane
// content and never sees the same lines in both history and pane.
type ConversationLog struct {
	mu           sync.Mutex
	stableLines  []string // finalized lines (append-only internally)
	currentPane  []string // volatile visible pane (replaced each ingest)
	inputHistory []string // prompts sent by user
	stableSeqNo  uint64   // bumped when stableLines grows
	lastStatus   string   // tracks status for transition detection
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
// a prefix of pane. This is bounded by pane size (terminal height, ~50 lines).
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

// SetStatus updates the instance status and promotes pane content to stable
// history on transition to "ready".
func (cl *ConversationLog) SetStatus(status string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	prev := cl.lastStatus
	cl.lastStatus = status
	if status == "ready" && prev != "ready" && prev != "" && len(cl.currentPane) > 0 {
		cl.stableLines = append(cl.stableLines, cl.currentPane...)
		cl.stableSeqNo++
	}
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
// Stable lines are trimmed to exclude any trailing overlap with the pane head,
// so the client never sees the same lines in both history and pane.
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
