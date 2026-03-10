package webserver

import (
	"fmt"
	"strings"
	"testing"
)

func TestConversationLog_FirstCapture(t *testing.T) {
	cl := NewConversationLog()

	// First capture: no scrollback, everything in pane
	cl.Ingest("line1\nline2\nline3", "line1\nline2\nline3")
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
}

func TestConversationLog_ScrollbackGrows(t *testing.T) {
	cl := NewConversationLog()

	// Initial: 3 lines all in pane
	cl.Ingest("line1\nline2\nline3", "line1\nline2\nline3")

	// Now 5 lines total, last 3 visible. Lines 1-2 are scrollback.
	cl.Ingest("line1\nline2\nline3\nline4\nline5", "line3\nline4\nline5")
	stable, seq, pane := cl.GetState()
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

func TestConversationLog_OverwriteRescue(t *testing.T) {
	cl := NewConversationLog()

	// Claude shows thinking output in the pane
	cl.Ingest("header\nthinking line 1\nthinking line 2\nprompt>",
		"header\nthinking line 1\nthinking line 2\nprompt>")

	// Claude replaces thinking with response (overwrites in-place)
	cl.Ingest("header\nresponse line 1\nresponse line 2\nprompt>",
		"header\nresponse line 1\nresponse line 2\nprompt>")

	stable, _, pane := cl.GetState()

	// The thinking lines should be rescued into stable
	if len(stable) != 2 {
		t.Errorf("expected 2 rescued stable lines, got %d: %v", len(stable), stable)
	}
	if len(stable) >= 2 {
		if stable[0] != "thinking line 1" || stable[1] != "thinking line 2" {
			t.Errorf("unexpected stable content: %v", stable)
		}
	}

	// Pane should have the current content
	if len(pane) != 4 {
		t.Errorf("expected 4 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_PromptLineNotRescued(t *testing.T) {
	cl := NewConversationLog()

	// Prompt line appears in both old and new → should NOT be rescued
	cl.Ingest("content A\nprompt>", "content A\nprompt>")
	cl.Ingest("content B\nprompt>", "content B\nprompt>")

	stable, _, _ := cl.GetState()

	// "content A" was overwritten → rescued
	// "prompt>" appears in both → not rescued
	if len(stable) != 1 {
		t.Errorf("expected 1 rescued line, got %d: %v", len(stable), stable)
	}
	if len(stable) >= 1 && stable[0] != "content A" {
		t.Errorf("expected 'content A', got %q", stable[0])
	}
}

func TestConversationLog_ScrollingDoesNotDuplicateRescue(t *testing.T) {
	cl := NewConversationLog()

	// Initial: 5 lines all in pane
	cl.Ingest("A\nB\nC\nD\nE", "A\nB\nC\nD\nE")

	// A, B scroll into scrollback. New lines appear.
	cl.Ingest("A\nB\nC\nD\nE\nF\nG", "C\nD\nE\nF\nG")

	stable, _, pane := cl.GetState()

	// A, B promoted via scrollback. C, D, E still in pane (matched in new full).
	// No lines should be rescued (nothing was overwritten).
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

func TestConversationLog_ScrollAndOverwrite(t *testing.T) {
	cl := NewConversationLog()

	// Old: [S1, thinking1, thinking2, prompt>]
	cl.Ingest("S1\nthinking1\nthinking2\nprompt>",
		"S1\nthinking1\nthinking2\nprompt>")

	// thinking1 scrolls into scrollback, thinking2 overwritten by response
	cl.Ingest("S1\nthinking1\nresponse1\nresponse2\nprompt>",
		"response1\nresponse2\nprompt>")

	stable, _, pane := cl.GetState()

	// S1, thinking1 from scrollback promotion
	// thinking2 from overwrite rescue
	if len(stable) != 3 {
		t.Errorf("expected 3 stable lines, got %d: %v", len(stable), stable)
	}
	if len(stable) >= 3 {
		if stable[0] != "S1" || stable[1] != "thinking1" || stable[2] != "thinking2" {
			t.Errorf("unexpected stable: %v", stable)
		}
	}
	if len(pane) != 3 {
		t.Errorf("expected 3 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestConversationLog_BlankLinesNotRescued(t *testing.T) {
	cl := NewConversationLog()

	cl.Ingest("content\n\n\nprompt>", "content\n\n\nprompt>")
	cl.Ingest("new content\nprompt>", "new content\nprompt>")

	stable, _, _ := cl.GetState()

	// "content" rescued, blank lines skipped
	if len(stable) != 1 {
		t.Errorf("expected 1 rescued line (blanks skipped), got %d: %v", len(stable), stable)
	}
	if len(stable) >= 1 && stable[0] != "content" {
		t.Errorf("expected 'content', got %q", stable[0])
	}
}

func TestConversationLog_GetStableSince(t *testing.T) {
	cl := NewConversationLog()

	cl.Ingest("a\nb\nc", "a\nb\nc")
	cl.Ingest("a\nb\nc\nd\ne", "d\ne")

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
	for total := paneSize; total <= 100; total += 10 {
		lines := make([]string, total)
		for i := range lines {
			lines[i] = fmt.Sprintf("line-%d", i+1)
		}
		full := strings.Join(lines, "\n")
		paneStart := total - paneSize
		if paneStart < 0 {
			paneStart = 0
		}
		pane := strings.Join(lines[paneStart:], "\n")
		cl.Ingest(full, pane)
	}

	stable, _, pane := cl.GetState()
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
	stable, _, pane := cl.GetState()
	if len(stable) != 3 {
		t.Errorf("expected 3 stable lines, got %d: %v", len(stable), stable)
	}
	if len(pane) != 0 {
		t.Errorf("expected 0 pane lines, got %d: %v", len(pane), pane)
	}
}

func TestFindOverwritten(t *testing.T) {
	tests := []struct {
		name    string
		oldFull []string
		newFull []string
		want    []string
	}{
		{
			name:    "no change",
			oldFull: []string{"A", "B", "C"},
			newFull: []string{"A", "B", "C"},
			want:    nil,
		},
		{
			name:    "pure append",
			oldFull: []string{"A", "B"},
			newFull: []string{"A", "B", "C"},
			want:    nil,
		},
		{
			name:    "overwrite in pane",
			oldFull: []string{"S1", "thinking", "prompt>"},
			newFull: []string{"S1", "response", "prompt>"},
			want:    []string{"thinking"},
		},
		{
			name:    "scrolling only",
			oldFull: []string{"A", "B", "C", "D"},
			newFull: []string{"A", "B", "C", "D", "E", "F"},
			want:    nil,
		},
		{
			name:    "blank lines skipped",
			oldFull: []string{"A", "", "  ", "B"},
			newFull: []string{"A", "C"},
			want:    []string{"B"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := findOverwritten(tt.oldFull, tt.newFull)
			if len(got) != len(tt.want) {
				t.Errorf("findOverwritten() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("findOverwritten()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
