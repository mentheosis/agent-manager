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

func TestConversationLog_ReadyPromotionDeduplicatedAtRead(t *testing.T) {
	cl := NewConversationLog()

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

	// Transition to ready: pane promoted to stable internally
	cl.SetStatus("ready")

	// GetState deduplicates: stable tail matches pane, so stable is trimmed
	// Pane is always returned as-is
	stable, _, pane, _ = cl.GetState()
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines (always real pane), got %d: %v", len(pane), pane)
	}
	// Stable should have the promoted content trimmed (it overlaps with pane)
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines (overlap trimmed), got %d: %v", len(stable), stable)
	}

	// Next tick: same pane recaptured — still deduplicated
	cl.Ingest("", "response line 1\nresponse line 2")
	stable, _, pane, _ = cl.GetState()
	if len(pane) != 2 {
		t.Errorf("expected 2 pane lines, got %d: %v", len(pane), pane)
	}
	if len(stable) != 0 {
		t.Errorf("expected 0 stable (still overlapping), got %d: %v", len(stable), stable)
	}

	// Pane content changes (new turn starts) — promoted lines now appear in stable
	cl.Ingest("", "new output from agent")
	stable, _, pane, _ = cl.GetState()
	if len(pane) != 1 {
		t.Errorf("expected 1 pane line, got %d: %v", len(pane), pane)
	}
	if len(stable) != 2 {
		t.Errorf("expected 2 stable lines (promoted, no longer overlapping), got %d: %v", len(stable), stable)
	}
	if len(stable) >= 2 && (stable[0] != "response line 1" || stable[1] != "response line 2") {
		t.Errorf("unexpected stable: %v", stable)
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

	cl.Ingest("", "⠋ Working (3s)")
	cl.Ingest("", "⠙ Working (4s)")
	cl.Ingest("", "⠹ Working (5s)")

	stable, _, _, _ := cl.GetState()
	if len(stable) != 0 {
		t.Errorf("expected 0 stable lines during running, got %d: %v", len(stable), stable)
	}

	// Transition to ready with result — promoted internally, but pane still shows it
	cl.Ingest("", "Done! Here is the result.")
	cl.SetStatus("ready")

	stable, _, pane, _ := cl.GetState()
	// Pane always has real content
	if len(pane) != 1 {
		t.Errorf("expected 1 pane line, got %d: %v", len(pane), pane)
	}
	// Stable trimmed because it overlaps with pane
	if len(stable) != 0 {
		t.Errorf("expected 0 stable (overlap trimmed), got %d: %v", len(stable), stable)
	}

	// Once pane changes, the promoted line appears in stable
	cl.Ingest("", "new prompt response")
	stable, _, pane, _ = cl.GetState()
	if len(stable) != 1 {
		t.Errorf("expected 1 stable line, got %d: %v", len(stable), stable)
	}
	if len(stable) >= 1 && stable[0] != "Done! Here is the result." {
		t.Errorf("unexpected stable: %v", stable)
	}
	if len(pane) != 1 {
		t.Errorf("expected 1 pane line, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_PartialOverlap(t *testing.T) {
	cl := NewConversationLog()

	// Simulate: 5 lines promoted to stable, then pane partially scrolls
	cl.SetStatus("running")
	cl.Ingest("", "A\nB\nC\nD\nE")
	cl.SetStatus("ready")

	// Pane still shows A-E, plus A,B scrolled to scrollback and F,G appeared
	cl.Ingest("A\nB", "C\nD\nE\nF\nG")

	stable, _, pane, _ := cl.GetState()

	// Pane is [C,D,E,F,G]. Stable internally is [A,B,C,D,E,A,B].
	// The tail of stable [A,B] does NOT match pane head [C,D], so no overlap trim.
	// But stable also has [C,D,E] from promotion that overlaps pane head.
	// findOverlap checks: does stable tail match pane head?
	// stable = [A,B,C,D,E,A,B], pane = [C,D,E,F,G]
	// overlap=5: [D,E,A,B] != [C,D,E,F,G][:5]... nope
	// overlap=3: [E,A,B] != [C,D,E]... nope
	// No overlap detected. This is correct because the lines have genuinely
	// diverged — the scrollback delta captured A,B at the end.

	if len(pane) != 5 {
		t.Errorf("expected 5 pane lines, got %d: %v", len(pane), pane)
	}
	// stable = [A,B,C,D,E] (promoted) + [A,B] (scrollback delta) = 7
	// No overlap with pane head, so all 7 returned
	if len(stable) != 7 {
		t.Errorf("expected 7 stable lines, got %d: %v", len(stable), stable)
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

func TestFindOverlap(t *testing.T) {
	tests := []struct {
		name    string
		stable  []string
		pane    []string
		want    int
	}{
		{"no overlap", []string{"A", "B"}, []string{"C", "D"}, 0},
		{"full overlap", []string{"A", "B", "C"}, []string{"A", "B", "C"}, 3},
		{"partial tail-head", []string{"X", "A", "B"}, []string{"A", "B", "C"}, 2},
		{"single line", []string{"X", "Y", "A"}, []string{"A", "B"}, 1},
		{"empty stable", []string{}, []string{"A"}, 0},
		{"empty pane", []string{"A"}, []string{}, 0},
		{"both empty", []string{}, []string{}, 0},
		{"no match despite same content exists", []string{"A", "B", "C"}, []string{"B", "C", "D"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findOverlap(tt.stable, tt.pane)
			if got != tt.want {
				t.Errorf("findOverlap(%v, %v) = %d, want %d", tt.stable, tt.pane, got, tt.want)
			}
		})
	}
}
