package webserver

import (
	"fmt"
	"strings"
	"testing"
)

func TestConversationLog_FirstCapture(t *testing.T) {
	cl := NewConversationLog()

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

	cl.Ingest("", "line1\nline2\nline3")
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

func TestConversationLog_ScrollingDoesNotDuplicate(t *testing.T) {
	cl := NewConversationLog()

	cl.Ingest("", "A\nB\nC\nD\nE")
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

	cl.Ingest("line1\nline2\nline3", "")
	stable, _, pane, _ := cl.GetState()
	if len(stable) != 3 {
		t.Errorf("expected 3 stable lines, got %d: %v", len(stable), stable)
	}
	if len(pane) != 0 {
		t.Errorf("expected 0 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_SetStatusDoesNotPromote(t *testing.T) {
	cl := NewConversationLog()

	// Simulate: agent produces output, then goes ready
	cl.SetStatus("running")
	cl.Ingest("", "response line 1\nresponse line 2")

	// During running: pane visible, stable empty
	stable, _, pane, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines during running, got %d: %v", len(stable), stable)
	}
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines, got %d: %v", len(pane), pane)
	}

	// Transition to ready: pane should NOT be promoted to stable
	cl.SetStatus("ready")

	stable, seq, pane, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines after ready (no promotion), got %d: %v", len(stable), stable)
	}
	if seq != 0 {
		t.Errorf("expected seq 0 (no promotion), got %d", seq)
	}
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_ShortResponseInPane(t *testing.T) {
	cl := NewConversationLog()

	// Short response that never scrolls off
	cl.SetStatus("running")
	cl.Ingest("", "Done!")
	cl.SetStatus("ready")

	// Response stays in pane only — not in stable
	stable, _, pane, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines (short response stays in pane), got %d: %v", len(stable), stable)
	}
	if len(pane) != 1 || pane[0] != "Done!" {
		t.Errorf("expected pane=['Done!'], got %v", pane)
	}

	// New turn starts: user sends prompt, new output appears
	// The old response scrolls off via scrollback delta
	cl.Ingest("Done!", "new output from agent")

	stable, _, pane, _ = cl.GetState()
	if len(stable) != 1 || stable[0] != "Done!" {
		t.Errorf("expected stable=['Done!'] from scrollback, got %v", stable)
	}
	if len(pane) != 1 || pane[0] != "new output from agent" {
		t.Errorf("expected pane=['new output from agent'], got %v", pane)
	}
}

func TestConversationLog_ReadyIdempotent(t *testing.T) {
	cl := NewConversationLog()

	cl.SetStatus("running")
	cl.Ingest("", "line1\nline2")
	cl.SetStatus("ready")

	stable1, seq1, _, _ := cl.GetState()

	// Repeated ready should not change anything
	cl.SetStatus("ready")
	stable2, seq2, _, _ := cl.GetState()

	if len(stable1) != len(stable2) {
		t.Errorf("repeated ready changed stable: %d -> %d", len(stable1), len(stable2))
	}
	if seq1 != seq2 {
		t.Errorf("repeated ready bumped seq: %d -> %d", seq1, seq2)
	}
}

func TestConversationLog_ScrollbackThenReady(t *testing.T) {
	// Simulates the scenario that previously caused duplication:
	// lines scroll off, then status goes ready. With scrollback-only model,
	// there's no promotion so no duplication.
	cl := NewConversationLog()

	cl.SetStatus("running")
	cl.Ingest("", "A\nB\nC\nD\nE")

	// Lines A,B scroll off
	cl.Ingest("A\nB", "C\nD\nE\nF\nG")

	// Status goes ready
	cl.SetStatus("ready")

	stable, _, pane, _ := cl.GetState()

	// Stable should only have scrollback lines [A,B] — no promotion of pane
	if len(stable) != 2 {
		t.Errorf("expected 2 stable lines (scrollback only), got %d: %v", len(stable), stable)
	}
	if len(stable) >= 2 && (stable[0] != "A" || stable[1] != "B") {
		t.Errorf("unexpected stable: %v", stable)
	}
	if len(pane) != 5 {
		t.Errorf("expected 5 pane lines, got %d: %v", len(pane), pane)
	}

	// Total lines should have no duplicates
	allLines := append(stable, pane...)
	if len(allLines) != 7 {
		t.Errorf("expected 7 total lines, got %d", len(allLines))
	}
	expected := []string{"A", "B", "C", "D", "E", "F", "G"}
	for i, line := range allLines {
		if i < len(expected) && line != expected[i] {
			t.Errorf("line %d: expected %q, got %q", i, expected[i], line)
		}
	}
}

func TestConversationLog_LastInput(t *testing.T) {
	cl := NewConversationLog()

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

	_, _, _, lastInput := cl.GetState()
	if lastInput != "add tests" {
		t.Errorf("GetState lastInput: expected 'add tests', got %q", lastInput)
	}
}

func TestConversationLog_TrimStable(t *testing.T) {
	cl := NewConversationLog()

	// Build up some stable lines
	cl.Ingest("A\nB\nC\nD\nE", "F\nG")

	if cl.GetStableCount() != 5 {
		t.Fatalf("expected 5 stable, got %d", cl.GetStableCount())
	}

	// Simulate resize: 2 lines return from scrollback to pane
	cl.TrimStable(2)

	stable, _, _, _ := cl.GetState()
	if len(stable) != 3 {
		t.Errorf("expected 3 stable after trim, got %d: %v", len(stable), stable)
	}
	if len(stable) >= 3 && (stable[0] != "A" || stable[1] != "B" || stable[2] != "C") {
		t.Errorf("unexpected stable after trim: %v", stable)
	}

	// Trim more than available
	cl.TrimStable(100)
	stable, _, _, _ = cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable after over-trim, got %d: %v", len(stable), stable)
	}

	// Trim zero does nothing
	cl.Ingest("X\nY", "Z")
	cl.TrimStable(0)
	if cl.GetStableCount() != 2 {
		t.Errorf("expected 2 stable after trim(0), got %d", cl.GetStableCount())
	}
}

func TestConversationLog_GetStableCount(t *testing.T) {
	cl := NewConversationLog()

	if cl.GetStableCount() != 0 {
		t.Errorf("expected 0, got %d", cl.GetStableCount())
	}

	cl.Ingest("a\nb\nc", "d\ne")
	if cl.GetStableCount() != 3 {
		t.Errorf("expected 3, got %d", cl.GetStableCount())
	}

	cl.Ingest("d", "e\nf")
	if cl.GetStableCount() != 4 {
		t.Errorf("expected 4, got %d", cl.GetStableCount())
	}
}
