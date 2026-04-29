package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	var (
		baseURL    = flag.String("base-url", "http://localhost:8787", "agent-manager API base URL")
		groupTitle = flag.String("group", "", "loop instance title to orchestrate (required)")
		mcpPort    = flag.Int("mcp-port", 0, "MCP HTTP server port (0 for stdio mode)")
	)
	flag.Parse()

	if *groupTitle == "" {
		fmt.Fprintln(os.Stderr, "Error: -group is required")
		flag.Usage()
		os.Exit(1)
	}

	mcp := NewMCPServer(*baseURL, *groupTitle)

	// Set up logging to stderr so stdout is clean for MCP stdio mode
	mcp.SetLogFunc(func(s string) {
		fmt.Fprintln(os.Stderr, s)
	})

	fmt.Fprintf(os.Stderr, "Starting orchestrator for group %q (base=%s, port=%d)\n",
		*groupTitle, *baseURL, *mcpPort)

	var err error
	if *mcpPort > 0 {
		// HTTP mode - for Claude Desktop or programmatic access
		err = mcp.RunHTTP(*mcpPort)
	} else {
		// stdio mode - for direct MCP connection
		err = mcp.Run()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
