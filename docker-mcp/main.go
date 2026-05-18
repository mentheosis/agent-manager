// docker-mcp is a tiny MCP server that exposes a fixed set of host-side
// commands (typically `docker build`, `docker compose`, etc.) to a Claude
// Agent SDK client running inside a container. The server is the security
// boundary: it can only run commands that the operator has pre-approved by
// listing them as named profiles in a JSON config file. Claude can invoke
// a profile by name but cannot inject arguments or change the working
// directory.
//
// Transports:
//   - HTTP (default): bearer-token-authenticated JSON-RPC POSTs. Bind to
//     127.0.0.1 by default; access from a Docker container via
//     host.docker.internal (or the Linux host-gateway IP).
//   - stdio: one JSON-RPC request per line. Useful for local tests and for
//     running the server as a subprocess of another agent.
//
// See README.md for a full setup walkthrough.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	var (
		configPath  = flag.String("config", "./config.json", "path to config JSON")
		stdio       = flag.Bool("stdio", false, "run in stdio mode instead of HTTP (for local tests)")
		listen      = flag.String("listen", "", "override config.listen (e.g. 127.0.0.1:9090)")
		allowNoAuth = flag.Bool("allow-no-auth", false, "permit running the HTTP server without a bearer token (UNSAFE; only for local dev)")
		printConfig = flag.Bool("print-config", false, "load and print the resolved config, then exit")
	)
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	if *printConfig {
		fmt.Fprintf(os.Stderr, "config from %s:\n", *configPath)
		fmt.Fprintf(os.Stderr, "  listen:                    %s\n", cfg.Listen)
		fmt.Fprintf(os.Stderr, "  auth_token_env:            %s\n", cfg.AuthTokenEnv)
		fmt.Fprintf(os.Stderr, "  log_dir:                   %s\n", cfg.LogDir)
		fmt.Fprintf(os.Stderr, "  max_concurrent_per_profile: %d\n", cfg.MaxConcurrentPerProfile)
		fmt.Fprintf(os.Stderr, "  profiles (%d):\n", len(cfg.Profiles))
		for _, p := range cfg.Profiles {
			fmt.Fprintf(os.Stderr, "    - %s\n", p.Name)
			fmt.Fprintf(os.Stderr, "        cwd:  %s\n", p.Cwd)
			fmt.Fprintf(os.Stderr, "        argv: %v\n", p.Argv)
			if p.TimeoutSeconds > 0 {
				fmt.Fprintf(os.Stderr, "        timeout: %ds\n", p.TimeoutSeconds)
			}
		}
		return
	}

	jobs, err := NewJobManager(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "job manager: %v\n", err)
		os.Exit(1)
	}

	mcp := NewMCPServer(cfg, jobs)
	mcp.SetLogFunc(func(s string) { fmt.Fprintln(os.Stderr, s) })

	if *stdio {
		fmt.Fprintf(os.Stderr, "docker-mcp: stdio mode, %d profile(s) loaded\n", len(cfg.Profiles))
		if err := mcp.ServeStdio(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "stdio error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// HTTP mode
	token := os.Getenv(cfg.AuthTokenEnv)
	if token == "" && !*allowNoAuth {
		fmt.Fprintf(os.Stderr, "error: %s is empty and --allow-no-auth was not set\n", cfg.AuthTokenEnv)
		fmt.Fprintf(os.Stderr, "set a token: export %s=$(openssl rand -hex 32)\n", cfg.AuthTokenEnv)
		os.Exit(1)
	}
	if token == "" {
		fmt.Fprintln(os.Stderr, "WARNING: running without authentication (--allow-no-auth). Anyone who can reach the listen address can run your profiles.")
	}

	httpSrv := NewHTTPServer(mcp, token)
	fmt.Fprintf(os.Stderr, "docker-mcp: HTTP mode on %s, %d profile(s) loaded\n", cfg.Listen, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		fmt.Fprintf(os.Stderr, "  - %-20s %v (cwd=%s)\n", p.Name, p.Argv, p.Cwd)
	}
	if err := httpSrv.ListenAndServe(cfg.Listen); err != nil {
		fmt.Fprintf(os.Stderr, "http error: %v\n", err)
		os.Exit(1)
	}
}
