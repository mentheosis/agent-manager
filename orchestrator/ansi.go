package orchestrator

// ANSI escape codes for terminal formatting.
// These are rendered by both tmux and the web UI's ansi.js parser.
const (
	ansiReset     = "\033[0m"
	ansiBold      = "\033[1m"
	ansiDim       = "\033[2m"
	ansiCyan      = "\033[36m"
	ansiGreen     = "\033[32m"
	ansiYellow    = "\033[33m"
	ansiBrightBlk = "\033[90m" // gray
)

// colorize wraps text in ANSI codes and resets after.
func colorize(code, text string) string {
	return code + text + ansiReset
}
