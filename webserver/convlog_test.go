package webserver

import (
	"fmt"
	"strings"
	"testing"
)

func TestConversationLog_FirstCapture(t *testing.T) {
	cl := NewConversationLog()

	// First capture: no scrollback, everything in pane
	cl.Ingest("", "line1\nline2\nline3")
	stable, seq, pane, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines, got %d", len(stable))
	}
	if seq != 0 {
		t.Errorf("expected seq 0, got %d", seq)
	}
	if len(pane) != 3 {
		t.Errorf("expected 3 pane lines, got %d", len(pane))
	}
}

func TestConversationLog_ScrollbackGrows(t *testing.T) {
	cl := NewConversationLog()

	// Initial: 3 lines all in pane, no scrollback
	cl.Ingest("", "line1\nline2\nline3")

	// Lines 1-2 scroll into scrollback. Pass them as the delta.
	cl.Ingest("line1\nline2", "line3\nline4\nline5")
	stable, seq, pane, _ := cl.GetState()
	if len(stable) != 2 {
		t.Errorf("expected 2 stable lines, got %d: %v", len(stable), stable)
	}
	if seq != 1 {
		t.Errorf("expected seq 1, got %d", seq)
	}
	if stable[0] != "line1" || stable[1] != "line2" {
		t.Errorf("unexpected stable: %v", stable)
	}
	if len(pane) != 3 {
		t.Errorf("expected 3 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_ScrollingDoesNotDuplicateRescue(t *testing.T) {
	cl := NewConversationLog()

	// Initial: 5 lines all in pane
	cl.Ingest("", "A\nB\nC\nD\nE")

	// A, B scroll into scrollback. Pass them as the delta.
	cl.Ingest("A\nB", "C\nD\nE\nF\nG")

	stable, _, pane, _ := cl.GetState()

	if len(stable) != 2 {
		t.Errorf("expected 2 stable (scrollback only), got %d: %v", len(stable), stable)
	}
	if len(stable) >= 2 && (stable[0] != "A" || stable[1] != "B") {
		t.Errorf("unexpected stable: %v", stable)
	}
	if len(pane) != 5 {
		t.Errorf("expected 5 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_GetStableSince(t *testing.T) {
	cl := NewConversationLog()

	cl.Ingest("", "a\nb\nc")
	// a, b, c all scroll off into scrollback
	cl.Ingest("a\nb\nc", "d\ne")

	delta := cl.GetStableSince(1)
	if len(delta) != 2 {
		t.Errorf("expected 2 delta lines, got %d: %v", len(delta), delta)
	}
	if len(delta) >= 2 && (delta[0] != "b" || delta[1] != "c") {
		t.Errorf("unexpected delta: %v", delta)
	}

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

func TestConversationLog_NoDuplication(t *testing.T) {
	cl := NewConversationLog()

	paneSize := 20
	prevScrollback := 0
	for total := paneSize; total <= 100; total += 10 {
		lines := make([]string, total)
		for i := range lines {
			lines[i] = fmt.Sprintf("line-%d", i+1)
		}
		paneStart := total - paneSize
		if paneStart < 0 {
			paneStart = 0
		}
		pane := strings.Join(lines[paneStart:], "\n")

		// Compute the scrollback delta: lines between prevScrollback and paneStart
		var scrollbackDelta string
		if paneStart > prevScrollback {
			scrollbackDelta = strings.Join(lines[prevScrollback:paneStart], "\n")
		}
		prevScrollback = paneStart

		cl.Ingest(scrollbackDelta, pane)
	}

	stable, _, pane, _ := cl.GetState()
	allLines := append(stable, pane...)
	if len(allLines) != 100 {
		t.Errorf("expected 100 total lines, got %d (stable=%d, pane=%d)", len(allLines), len(stable), len(pane))
	}
	for i, line := range allLines {
		expected := fmt.Sprintf("line-%d", i+1)
		if line != expected {
			t.Errorf("line %d: expected %q, got %q", i, expected, line)
			break
		}
	}
}

func TestConversationLog_EmptyPane(t *testing.T) {
	cl := NewConversationLog()

	// All lines passed as scrollback delta (pane is empty)
	cl.Ingest("line1\nline2\nline3", "")
	stable, _, pane, _ := cl.GetState()
	if len(stable) != 3 {
		t.Errorf("expected 3 stable lines, got %d: %v", len(stable), stable)
	}
	if len(pane) != 0 {
		t.Errorf("expected 0 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_TurnBasedPromotion(t *testing.T) {
	cl := NewConversationLog()

	// Simulate running state with pane content
	cl.SetStatus("running")
	cl.Ingest("", "response line 1\nresponse line 2")

	// Pane should have content, stable should be empty
	stable, _, pane, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines during running, got %d: %v", len(stable), stable)
	}
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines, got %d: %v", len(pane), pane)
	}

	// Transition to ready — pane should be promoted to stable
	cl.SetStatus("ready")
	stable, seq, pane, _ := cl.GetState()
	if len(stable) != 2 {
		t.Errorf("expected 2 stable lines after ready, got %d: %v", len(stable), stable)
	}
	if seq != 1 {
		t.Errorf("expected seq 1, got %d", seq)
	}
	if stable[0] != "response line 1" || stable[1] != "response line 2" {
		t.Errorf("unexpected stable content: %v", stable)
	}
	if len(pane) != 0 {
		t.Errorf("expected 0 pane lines after promotion, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_ReadyIdempotent(t *testing.T) {
	cl := NewConversationLog()

	cl.SetStatus("running")
	cl.Ingest("", "line1\nline2")
	cl.SetStatus("ready")

	stable1, seq1, _, _ := cl.GetState()

	// Repeated ready should not duplicate
	cl.SetStatus("ready")
	stable2, seq2, _, _ := cl.GetState()

	if len(stable1) != len(stable2) {
		t.Errorf("repeated ready changed stable: %d -> %d", len(stable1), len(stable2))
	}
	if seq1 != seq2 {
		t.Errorf("repeated ready bumped seq: %d -> %d", seq1, seq2)
	}
}

func TestConversationLog_TimerLinesNotPromoted(t *testing.T) {
	cl := NewConversationLog()

	cl.SetStatus("running")

	// Simulate timer lines being rewritten during running (no scrollback)
	cl.Ingest("", "⠋ Working (3s)")
	cl.Ingest("", "⠙ Working (4s)")
	cl.Ingest("", "⠹ Working (5s)")

	stable, _, _, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines during running, got %d: %v", len(stable), stable)
	}

	// Now transition to ready with actual content
	cl.Ingest("", "Done! Here is the result.")
	cl.SetStatus("ready")

	stable, _, _, _ = cl.GetState()
	if len(stable) != 1 {
		t.Errorf("expected 1 stable line after ready, got %d: %v", len(stable), stable)
	}
	if len(stable) >= 1 && stable[0] != "Done! Here is the result." {
		t.Errorf("unexpected stable content: %v", stable)
	}
}

func TestConversationLog_LastInput(t *testing.T) {
	cl := NewConversationLog()

	// Initially empty
	if got := cl.GetLastInput(); got != "" {
		t.Errorf("expected empty last input, got %q", got)
	}

	cl.SetLastInput("fix the bug")
	if got := cl.GetLastInput(); got != "fix the bug" {
		t.Errorf("expected 'fix the bug', got %q", got)
	}

	cl.SetLastInput("add tests")
	if got := cl.GetLastInput(); got != "add tests" {
		t.Errorf("expected 'add tests', got %q", got)
	}

	// Verify it's included in GetState
	_, _, _, lastInput := cl.GetState()
	if lastInput != "add tests" {
		t.Errorf("GetState lastInput: expected 'add tests', got %q", lastInput)
	}
}
