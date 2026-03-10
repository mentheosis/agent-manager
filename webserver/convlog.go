package webserver

import (
	"strings"
	"sync"
)

// ConversationLog accumulates terminal output by separating tmux scrollback
// (stable, append-only) from the visible pane (volatile, constantly rewritten).
//
// Two mechanisms preserve output:
//  1. Scrollback promotion: lines that scroll off the visible area into tmux
//     scrollback are appended to stableLines.
//  2. Overwrite rescue: lines that were in the previous full capture but are
//     absent from the new full capture were overwritten in-place (e.g. thinking
//     output replaced by response). These are rescued into stableLines so they
//     are not lost.
type ConversationLog struct {
	mu            sync.Mutex
	stableLines   []string // finalized lines (append-only)
	lastFullLines []string // previous full capture (for overwrite detection)
	currentPane   []string // volatile visible pane (replaced each ingest)
	inputHistory  []string // prompts sent by user
	stableSeqNo   uint64   // bumped when stableLines grows
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

// Ingest processes a new pair of tmux captures:
//   - fullCapture: entire output (scrollback + visible pane), from capture-pane -S - -E -
//   - paneCapture: just the visible pane, from capture-pane (no -S/-E)
func (cl *ConversationLog) Ingest(fullCapture string, paneCapture string) {
	cl.mu.Lock()
	defer cl.mu.Unlock()

	fullLines := splitLines(fullCapture)
	paneLines := splitLines(paneCapture)

	if len(fullLines) == 0 {
		cl.currentPane = nil
		return
	}

	// The visible pane is the last N lines of the full capture.
	// Everything before that is scrollback (stable).
	scrollbackEnd := len(fullLines) - len(paneLines)
	if scrollbackEnd < 0 {
		scrollbackEnd = 0
	}

	// Step 1: Promote new scrollback lines.
	if scrollbackEnd > len(cl.stableLines) {
		cl.stableLines = append(cl.stableLines, fullLines[len(cl.stableLines):scrollbackEnd]...)
		cl.stableSeqNo++
	}

	// Step 2: Rescue overwritten pane content.
	// Compare previous full capture with current to find lines that disappeared.
	// These were overwritten in-place by Claude Code (e.g. thinking → response).
	if len(cl.lastFullLines) > 0 {
		rescued := findOverwritten(cl.lastFullLines, fullLines)
		if len(rescued) > 0 {
			cl.stableLines = append(cl.stableLines, rescued...)
			cl.stableSeqNo++
		}
	}

	cl.currentPane = paneLines
	cl.lastFullLines = fullLines
}

// findOverwritten compares two successive full captures and returns lines from
// oldFull that are not present in newFull (overwritten content).
//
// Algorithm:
//  1. Find the common prefix (unchanged scrollback).
//  2. Lines after the prefix form the "tail" of each capture.
//  3. Use multiset subtraction: lines in oldTail that can't be matched to a
//     line in newTail were overwritten and should be rescued.
//
// This correctly handles scrolling (matched lines), duplicate lines, and
// in-place rewrites. The prompt line typically appears in both tails and
// gets matched, so it is NOT rescued.
func findOverwritten(oldFull, newFull []string) []string {
	// Find common prefix (unchanged scrollback region)
	prefixLen := 0
	maxPrefix := len(oldFull)
	if len(newFull) < maxPrefix {
		maxPrefix = len(newFull)
	}
	for prefixLen < maxPrefix && oldFull[prefixLen] == newFull[prefixLen] {
		prefixLen++
	}

	oldTail := oldFull[prefixLen:]
	newTail := newFull[prefixLen:]

	if len(oldTail) == 0 {
		return nil
	}

	// Build multiset of new tail lines
	newSet := make(map[string]int, len(newTail))
	for _, line := range newTail {
		newSet[line]++
	}

	// Lines in old tail not matchable in new tail were overwritten
	var rescued []string
	for _, line := range oldTail {
		if newSet[line] > 0 {
			newSet[line]--
		} else {
			// Only rescue lines with actual content
			if strings.TrimSpace(line) != "" {
				rescued = append(rescued, line)
			}
		}
	}

	return rescued
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
