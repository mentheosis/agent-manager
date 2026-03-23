package webserver

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// PaneAction represents an interactive option or shortcut detected in a tmux pane.
type PaneAction struct {
	Type  string `json:"type"`  // "option" or "shortcut"
	Label string `json:"label"` // display label, e.g. "1. Yes" or "Esc (cancel)"
	Key   string `json:"key"`   // keystroke to send, e.g. "1" or "\x1b"
}

// PanePrompt represents the full interactive state of a pane: the question
// being asked and the available actions.
type PanePrompt struct {
	// Message is the context/question the model is asking about.
	Message string       `json:"message"`
	Actions []PaneAction `json:"actions"`
}

// AgentMeta is the enriched status information broadcast via the status websocket.
type AgentMeta struct {
	Status string      `json:"status"`
	Prompt *PanePrompt `json:"prompt,omitempty"` // nil when no interactive prompt
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// stripANSI removes ANSI escape sequences from a string.
func stripANSI(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

var optionLineRe = regexp.MustCompile(`^\s*([❯>›])?\s*(\d+)\.\s*(.+)`)
var hintRe = regexp.MustCompile(`(?i)(Esc|Tab|ctrl\+\w)\s+to\s+(\w+)`)

// parsePaneActions extracts interactive options and shortcut hints from pane content.
// Returns nil if no interactive prompt is detected.
func parsePaneActions(rawContent string) *PanePrompt {
	plain := stripANSI(rawContent)
	lines := strings.Split(plain, "\n")

	var actions []PaneAction
	var promptMessage string

	// --- Numbered options ---
	type optionEntry struct {
		num     string
		label   string
		lineIdx int
		cursor  bool
	}

	var allOptions []optionEntry
	cursorOptIdx := -1

	for i, line := range lines {
		m := optionLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		label := m[3]
		// Trim trailing whitespace/noise
		if idx := strings.Index(label, "   "); idx >= 0 {
			label = label[:idx]
		}
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		hasCursor := m[1] != ""
		if hasCursor {
			cursorOptIdx = len(allOptions)
		}
		allOptions = append(allOptions, optionEntry{
			num:     m[2],
			label:   label,
			lineIdx: i,
			cursor:  hasCursor,
		})
	}

	if cursorOptIdx >= 0 {
		// Walk forward from cursor to collect the group (gap ≤ 15 lines)
		groupEnd := cursorOptIdx
		for i := cursorOptIdx + 1; i < len(allOptions); i++ {
			if allOptions[i].lineIdx-allOptions[i-1].lineIdx <= 15 {
				groupEnd = i
			} else {
				break
			}
		}

		for i := cursorOptIdx; i <= groupEnd; i++ {
			opt := allOptions[i]
			actions = append(actions, PaneAction{
				Type:  "option",
				Label: fmt.Sprintf("%s. %s", opt.num, opt.label),
				Key:   opt.num,
			})
		}

		// Extract the prompt message: non-empty lines above the first option in the group
		promptMessage = extractPromptMessage(lines, allOptions[cursorOptIdx].lineIdx)
	}

	// --- Shortcut hints ---
	matches := hintRe.FindAllStringSubmatch(plain, -1)
	for _, m := range matches {
		key := m[1]
		verb := strings.ToLower(m[2])
		var keystroke string

		switch {
		case strings.EqualFold(key, "Esc"):
			keystroke = "\x1b"
		case strings.EqualFold(key, "Tab"):
			keystroke = "\x1b[Z" // shift-tab
		case strings.HasPrefix(strings.ToLower(key), "ctrl+"):
			letter := strings.ToLower(key[5:])
			if len(letter) == 1 {
				keystroke = string(rune(letter[0] - 'a' + 1))
			}
		}

		if keystroke != "" {
			actions = append(actions, PaneAction{
				Type:  "shortcut",
				Label: fmt.Sprintf("%s (%s)", key, verb),
				Key:   keystroke,
			})
		}

		// If we have shortcuts but no prompt message yet, grab context
		if promptMessage == "" && len(matches) > 0 {
			promptMessage = extractShortcutContext(lines)
		}
	}

	if len(actions) == 0 {
		return nil
	}

	return &PanePrompt{
		Message: promptMessage,
		Actions: actions,
	}
}

// extractPromptMessage grabs the context lines above the first option.
// It walks backwards from optionStartLine, collecting non-empty lines
// until it hits a blank line gap or the top of the pane.
func extractPromptMessage(lines []string, optionStartLine int) string {
	var contextLines []string
	blankCount := 0

	for i := optionStartLine - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			blankCount++
			if blankCount >= 2 {
				break // two consecutive blanks = end of context
			}
			continue
		}
		blankCount = 0
		contextLines = append(contextLines, trimmed)

		// Don't grab too many lines
		if len(contextLines) >= 10 {
			break
		}
	}

	// Reverse to restore top-to-bottom order
	for i, j := 0, len(contextLines)-1; i < j; i, j = i+1, j-1 {
		contextLines[i], contextLines[j] = contextLines[j], contextLines[i]
	}

	return strings.Join(contextLines, "\n")
}

// extractShortcutContext grabs the last meaningful content line before shortcut hints.
func extractShortcutContext(lines []string) string {
	// Walk from bottom, skip empty and hint lines
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		// Skip hint lines themselves
		if hintRe.MatchString(trimmed) {
			continue
		}
		// Skip lines that are just whitespace/decoration
		allSymbols := true
		for _, r := range trimmed {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				allSymbols = false
				break
			}
		}
		if allSymbols {
			continue
		}
		return trimmed
	}
	return ""
}
