package webserver

import (
	"strings"
	"sync"
)

// ConversationLog accumulates terminal output by diffing successive full tmux captures.
// It separates "stable" (finalized, scrolled-off) lines from "volatile" (current pane) lines,
// eliminating duplication and preserving output that gets overwritten in-place.
type ConversationLog struct {
	mu            sync.Mutex
	stableLines   []string // finalized lines (append-only)
	lastFullLines []string // all lines from last capture (for prefix comparison)
	currentPane   []string // volatile tail (current visible pane)
	inputHistory  []string // prompts sent by user
	stableSeqNo   uint64   // bumped when stableLines grows
}

// NewConversationLog creates a new empty ConversationLog.
func NewConversationLog() *ConversationLog {
	return &ConversationLog{}
}

// Ingest processes a new full tmux capture (scrollback + visible pane).
// It diffs against the previous capture to determine which lines are newly finalized.
func (cl *ConversationLog) Ingest(fullCapture string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	// Split into lines, strip trailing blank lines
	newLines := strings.Split(fullCapture, "\n")
	for len(newLines) > 0 && strings.TrimSpace(newLines[len(newLines)-1]) == "" {
		newLines = newLines[:len(newLines)-1]
	}

	if len(newLines) == 0 {
		cl.currentPane = nil
		return
	}

	if len(cl.lastFullLines) == 0 {
		// First capture: everything is volatile
		cl.currentPane = newLines
		cl.lastFullLines = newLines
		return
	}

	// Find common prefix length between lastFullLines and newLines
	prefixLen := 0
	maxPrefix := len(cl.lastFullLines)
	if len(newLines) < maxPrefix {
		maxPrefix = len(newLines)
	}
	for prefixLen < maxPrefix && cl.lastFullLines[prefixLen] == newLines[prefixLen] {
		prefixLen++
	}

	// Lines in the prefix that extend past current stableLines are newly finalized
	if prefixLen > len(cl.stableLines) {
		cl.stableLines = append(cl.stableLines, cl.lastFullLines[len(cl.stableLines):prefixLen]...)
		cl.stableSeqNo++
	}

	// Everything after the stable prefix is volatile (current pane)
	if prefixLen < len(newLines) {
		cl.currentPane = newLines[prefixLen:]
	} else {
		cl.currentPane = nil
	}

	cl.lastFullLines = newLines
}

// GetState returns the current stable lines, sequence number, and volatile pane.
func (cl *ConversationLog) GetState() (stableLines []string, stableSeqNo uint64, pane []string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	stable := make([]string, len(cl.stableLines))
	copy(stable, cl.stableLines)

	paneOut := make([]string, len(cl.currentPane))
	copy(paneOut, cl.currentPane)

	return stable, cl.stableSeqNo, paneOut
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
