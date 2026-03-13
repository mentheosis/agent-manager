package main

import (
	"bufio"
	"claude-squad/orchestrator"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	groupFlag := flag.String("group", "", "Orchestrator group title (required)")
	baseURLFlag := flag.String("base-url", "http://localhost:8080", "Web server URL")
	taskFlag := flag.String("task", "", "Initial task for the orchestrator")
	flag.Parse()

	if *groupFlag == "" {
		fmt.Fprintln(os.Stderr, "error: --group is required")
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\nShutting down orchestrator...")
		cancel()
	}()

	// Print banner
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║        Orchestrator Control Loop         ║")
	fmt.Println("╚══════════════════════════════════════════╝")
	fmt.Printf("  Group:    %s\n", *groupFlag)
	fmt.Printf("  API:      %s\n", *baseURLFlag)
	fmt.Println("  ─────────────────────────────────────────")

	// Start MCP server on Unix socket
	socketPath := fmt.Sprintf("/tmp/claude-squad-mcp-%s.sock", sanitizeForSocket(*groupFlag))
	mcpServer := orchestrator.NewMCPServer(*baseURLFlag, *groupFlag)
	go func() {
		fmt.Printf("  MCP socket: %s\n", socketPath)
		if err := mcpServer.RunOnSocket(socketPath); err != nil && ctx.Err() == nil {
			fmt.Printf("  MCP server error: %v\n", err)
		}
	}()
	defer os.Remove(socketPath)

	// Create and configure the loop
	cfg := orchestrator.DefaultConfig()
	cfg.BaseURL = *baseURLFlag
	loop := orchestrator.NewLoop(cfg, *groupFlag)

	// Start stdin command reader
	go readCommands(ctx, loop)

	fmt.Println()

	// Run the loop (blocking)
	if err := loop.Run(ctx, *taskFlag); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "loop error: %v\n", err)
		os.Exit(1)
	}
}

// readCommands reads control commands from stdin (sent by the web UI via tmux send-keys).
func readCommands(ctx context.Context, loop *orchestrator.Loop) {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "__PAUSE__":
			loop.Pause()
		case line == "__RESUME__":
			loop.Resume()
		case strings.HasPrefix(line, "__TASK__ "):
			taskJSON := strings.TrimPrefix(line, "__TASK__ ")
			var task string
			if err := json.Unmarshal([]byte(taskJSON), &task); err != nil {
				// Try using the raw string if JSON decode fails
				task = taskJSON
			}
			loop.Restart(task)
		}
	}
}

// sanitizeForSocket creates a safe filename from a group title.
func sanitizeForSocket(title string) string {
	r := strings.NewReplacer(" ", "-", "/", "-", "'", "", "\"", "")
	return r.Replace(title)
}
