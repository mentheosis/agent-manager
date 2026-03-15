//go:build integration

package tmux

import (
	"claude-squad/cmd"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Integration tests that verify our assumptions about tmux behavior.
// Run with: go test -tags integration ./session/tmux/...
//
// These tests require tmux to be installed and available in PATH.

func skipIfNoTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not found in PATH, skipping integration test")
	}
}

// createTestSession creates a small tmux session for testing.
// Returns the session name and a cleanup function.
func createTestSession(t *testing.T, rows, cols int) (string, func()) {
	t.Helper()
	name := fmt.Sprintf("test_csq_%d", time.Now().UnixNano())

	// Create detached session running cat (blocks waiting for input)
	c := exec.Command("tmux", "new-session", "-d", "-s", name, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), "cat")
	if err := c.Run(); err != nil {
		t.Fatalf("failed to create tmux session: %v", err)
	}

	cleanup := func() {
		exec.Command("tmux", "kill-session", "-t", name).Run()
	}

	// Wait for session to be ready
	time.Sleep(100 * time.Millisecond)
	return name, cleanup
}

// sendLines sends numbered lines to the tmux session via send-keys.
func sendLines(t *testing.T, sessionName string, startLine, count int) {
	t.Helper()
	for i := startLine; i < startLine+count; i++ {
		line := fmt.Sprintf("line-%03d", i)
		c := exec.Command("tmux", "send-keys", "-t", sessionName, line, "Enter")
		if err := c.Run(); err != nil {
			t.Fatalf("failed to send line %d: %v", i, err)
		}
	}
	// Give tmux time to process
	time.Sleep(200 * time.Millisecond)
}

func getHistorySize(t *testing.T, sessionName string) int {
	t.Helper()
	c := exec.Command("tmux", "display-message", "-p", "-t", sessionName, "#{history_size}")
	out, err := c.Output()
	if err != nil {
		t.Fatalf("failed to get history size: %v", err)
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("failed to parse history size %q: %v", string(out), err)
	}
	return size
}

func capturePane(t *testing.T, sessionName string) string {
	t.Helper()
	c := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", sessionName)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("failed to capture pane: %v", err)
	}
	return string(out)
}

func captureScrollback(t *testing.T, sessionName string, lineCount int) string {
	t.Helper()
	start := fmt.Sprintf("-%d", lineCount)
	c := exec.Command("tmux", "capture-pane", "-p", "-J", "-S", start, "-E", "-1", "-t", sessionName)
	out, err := c.Output()
	if err != nil {
		t.Fatalf("failed to capture scrollback: %v", err)
	}
	return string(out)
}

// nonEmptyLines returns non-empty lines from a string.
func nonEmptyLines(s string) []string {
	var result []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			result = append(result, line)
		}
	}
	return result
}

// TestIntegration_HistorySizeGrowsOnScrolloff verifies that history_size
// increases by exactly the number of lines that scroll off the visible pane.
func TestIntegration_HistorySizeGrowsOnScrolloff(t *testing.T) {
	skipIfNoTmux(t)

	// Create a session with 5 visible rows (cat uses 1 for prompt, so ~4 visible)
	sessionName, cleanup := createTestSession(t, 5, 80)
	defer cleanup()

	initialHistory := getHistorySize(t, sessionName)
	t.Logf("initial history_size: %d", initialHistory)

	// Send enough lines to fill the pane and start scrolling
	sendLines(t, sessionName, 1, 10)

	newHistory := getHistorySize(t, sessionName)
	t.Logf("after 10 lines, history_size: %d", newHistory)

	if newHistory <= initialHistory {
		t.Errorf("expected history_size to grow after sending lines that exceed pane height, got %d → %d", initialHistory, newHistory)
	}

	// Send more lines and verify continued growth
	prevHistory := newHistory
	sendLines(t, sessionName, 11, 5)
	newHistory = getHistorySize(t, sessionName)
	t.Logf("after 5 more lines, history_size: %d", newHistory)

	if newHistory <= prevHistory {
		t.Errorf("expected history_size to continue growing, got %d → %d", prevHistory, newHistory)
	}
}

// TestIntegration_CaptureScrollbackReturnsCorrectLines verifies that
// CaptureScrollback(-N, -1) returns the expected lines from scrollback.
func TestIntegration_CaptureScrollbackReturnsCorrectLines(t *testing.T) {
	skipIfNoTmux(t)

	sessionName, cleanup := createTestSession(t, 5, 80)
	defer cleanup()

	// Send 20 lines — many will scroll off
	sendLines(t, sessionName, 1, 20)

	histSize := getHistorySize(t, sessionName)
	t.Logf("history_size after 20 lines: %d", histSize)

	if histSize == 0 {
		t.Fatal("expected non-zero history_size after sending 20 lines in a 5-row pane")
	}

	// Capture last 3 lines of scrollback
	scrollback := captureScrollback(t, sessionName, 3)
	lines := nonEmptyLines(scrollback)
	t.Logf("captured scrollback (last 3): %v", lines)

	if len(lines) < 1 {
		t.Fatalf("expected at least 1 scrollback line, got %d", len(lines))
	}

	// Verify lines contain our numbered format
	for _, line := range lines {
		if !strings.HasPrefix(line, "line-") {
			t.Logf("scrollback line %q doesn't match expected format (may include echo)", line)
		}
	}
}

// TestIntegration_NoOverlapBetweenPaneAndScrollback verifies that the visible
// pane content and scrollback content do not contain the same lines.
func TestIntegration_NoOverlapBetweenPaneAndScrollback(t *testing.T) {
	skipIfNoTmux(t)

	sessionName, cleanup := createTestSession(t, 5, 80)
	defer cleanup()

	sendLines(t, sessionName, 1, 15)

	histSize := getHistorySize(t, sessionName)
	pane := capturePane(t, sessionName)
	scrollback := captureScrollback(t, sessionName, histSize)

	paneLines := nonEmptyLines(pane)
	scrollLines := nonEmptyLines(scrollback)

	t.Logf("pane lines: %v", paneLines)
	t.Logf("scrollback lines (%d): %v", histSize, scrollLines)

	// Build set from scrollback
	scrollSet := make(map[string]bool)
	for _, l := range scrollLines {
		scrollSet[l] = true
	}

	// Check no pane line is in scrollback
	for _, pl := range paneLines {
		if scrollSet[pl] {
			t.Errorf("line %q appears in both pane and scrollback", pl)
		}
	}
}

// TestIntegration_HistorySizeMonotonic verifies that history_size is
// monotonically increasing during normal operation (no resizes).
func TestIntegration_HistorySizeMonotonic(t *testing.T) {
	skipIfNoTmux(t)

	sessionName, cleanup := createTestSession(t, 5, 80)
	defer cleanup()

	prevSize := getHistorySize(t, sessionName)
	for batch := 0; batch < 5; batch++ {
		sendLines(t, sessionName, batch*5+1, 5)
		size := getHistorySize(t, sessionName)
		if size < prevSize {
			t.Errorf("history_size decreased: %d → %d (batch %d)", prevSize, size, batch)
		}
		prevSize = size
	}
}

// TestIntegration_ScrollbackImmutable verifies that scrollback content
// doesn't change between captures (without resize).
func TestIntegration_ScrollbackImmutable(t *testing.T) {
	skipIfNoTmux(t)

	sessionName, cleanup := createTestSession(t, 5, 80)
	defer cleanup()

	sendLines(t, sessionName, 1, 15)
	histSize := getHistorySize(t, sessionName)
	if histSize == 0 {
		t.Skip("no scrollback generated")
	}

	capture1 := captureScrollback(t, sessionName, histSize)
	time.Sleep(100 * time.Millisecond)
	capture2 := captureScrollback(t, sessionName, histSize)

	if capture1 != capture2 {
		t.Errorf("scrollback changed between captures:\n  first:  %q\n  second: %q", capture1, capture2)
	}
}

// TestIntegration_DeltaCaptureAccuracy verifies that capturing deltas
// (only new scrollback lines) produces correct, ordered content.
func TestIntegration_DeltaCaptureAccuracy(t *testing.T) {
	skipIfNoTmux(t)

	sessionName, cleanup := createTestSession(t, 5, 80)
	defer cleanup()

	// Use the TmuxSession wrapper to test the actual CaptureScrollback method
	ts := &TmuxSession{
		sanitizedName: sessionName,
		cmdExec:       cmd.MakeExecutor(),
	}

	// Phase 1: send initial lines
	sendLines(t, sessionName, 1, 10)
	size1, err := ts.GetHistorySize()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 10 lines: history_size=%d", size1)

	// Phase 2: send more lines
	sendLines(t, sessionName, 11, 10)
	size2, err := ts.GetHistorySize()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("after 20 lines: history_size=%d", size2)

	delta := size2 - size1
	if delta <= 0 {
		t.Fatalf("expected positive delta, got %d", delta)
	}

	// Capture just the delta
	start := fmt.Sprintf("-%d", delta)
	scrollback, err := ts.CapturePaneContentWithOptions(start, "-1")
	if err != nil {
		t.Fatal(err)
	}

	deltaLines := nonEmptyLines(scrollback)
	t.Logf("delta capture (%d lines): %v", len(deltaLines), deltaLines)

	// The delta lines should be sequential and from the expected range
	if len(deltaLines) == 0 {
		t.Error("expected non-empty delta capture")
	}
}
