package webserver

import (
	"strings"
	"testing"
)

func TestConversationLog_BasicIngest(t *testing.T) {
	cl := NewConversationLog()

	// First capture: everything is volatile
	cl.Ingest("line1\nline2\nline3")
	stable, seq, pane := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines, got %d", len(stable))
	}
	if seq != 0 {
		t.Errorf("expected seq 0, got %d", seq)
	}
	if len(pane) != 3 {
		t.Errorf("expected 3 pane lines, got %d", len(pane))
	}

	// Second capture: same prefix + new content. Shared prefix becomes stable.
	cl.Ingest("line1\nline2\nline3\nline4\nline5")
	stable, seq, pane = cl.GetState()
	if len(stable) != 3 {
		t.Errorf("expected 3 stable lines, got %d: %v", len(stable), stable)
	}
	if seq != 1 {
		t.Errorf("expected seq 1, got %d", seq)
	}
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines, got %d: %v", len(pane), pane)
	}
	if pane[0] != "line4" || pane[1] != "line5" {
		t.Errorf("unexpected pane content: %v", pane)
	}
}

func TestConversationLog_OverwrittenLines(t *testing.T) {
	cl := NewConversationLog()

	// Simulate progress bar: lines get overwritten
	cl.Ingest("header\nprogress: 10%")
	cl.Ingest("header\nprogress: 50%")

	stable, _, pane := cl.GetState()
	// "header" should be stable since it's shared prefix
	if len(stable) != 1 || stable[0] != "header" {
		t.Errorf("expected stable=[header], got %v", stable)
	}
	// Current volatile should be the latest progress
	if len(pane) != 1 || pane[0] != "progress: 50%" {
		t.Errorf("expected pane=[progress: 50%%], got %v", pane)
	}
}

func TestConversationLog_GetStableSince(t *testing.T) {
	cl := NewConversationLog()

	cl.Ingest("a\nb\nc")
	cl.Ingest("a\nb\nc\nd\ne")

	// Stable should be [a, b, c]
	delta := cl.GetStableSince(1)
	if len(delta) != 2 {
		t.Errorf("expected 2 delta lines, got %d: %v", len(delta), delta)
	}
	if delta[0] != "b" || delta[1] != "c" {
		t.Errorf("unexpected delta: %v", delta)
	}

	// Request from beyond stable count
	delta = cl.GetStableSince(10)
	if len(delta) != 0 {
		t.Errorf("expected 0 delta lines, got %d", len(delta))
	}
}

func TestConversationLog_InputHistory(t *testing.T) {
	cl := NewConversationLog()

	cl.AddInput("hello")
	cl.AddInput("world")
	history := cl.GetInputHistory()
	if len(history) != 2 {
		t.Fatalf("expected 2 history items, got %d", len(history))
	}
	if history[0] != "hello" || history[1] != "world" {
		t.Errorf("unexpected history: %v", history)
	}
}

func TestConversationLog_TrailingBlanksStripped(t *testing.T) {
	cl := NewConversationLog()

	cl.Ingest("line1\nline2\n\n\n")
	_, _, pane := cl.GetState()
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines (trailing blanks stripped), got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_NoDuplication(t *testing.T) {
	cl := NewConversationLog()

	// Simulate the exact scenario: scrollback + pane overlap
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = strings.Repeat("x", i+1)
	}

	// First capture
	cl.Ingest(strings.Join(lines[:50], "\n"))

	// Second capture extends
	cl.Ingest(strings.Join(lines[:80], "\n"))

	stable, _, pane := cl.GetState()

	// All lines from stable + pane should cover lines[:80] with no duplication
	allLines := append(stable, pane...)
	if len(allLines) != 80 {
		t.Errorf("expected 80 total lines, got %d (stable=%d, pane=%d)", len(allLines), len(stable), len(pane))
	}
}
