package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:8787", "agent-manager API base URL")
	group := flag.String("group", "", "parent team title (required)")
	mode := flag.String("mode", "mcp", "team control loop or standalone mcp tools")
	port := flag.Int("mcp-port", 0, "localhost HTTP port (required for team mode; 0 uses stdio in mcp mode)")
	task := flag.String("task", "", "initial team task")
	managed := flag.Bool("managed", false, "forward MCP completion to the managed team controller")
	flag.Parse()
	if *group == "" || (*mode != "team" && *mode != "mcp") || (*mode == "team" && *port <= 0) {
		fmt.Fprintln(os.Stderr, "Require --group and --mode mcp|team; team mode requires --mcp-port")
		os.Exit(1)
	}
	mcp := NewMCPServer(*baseURL, *group)
	mcp.managed = *managed
	log := func(s string) { fmt.Fprintln(os.Stderr, s) }
	mcp.SetLogFunc(log)
	var err error
	if *mode == "team" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		cfg := DefaultConfig()
		cfg.BaseURL = *baseURL
		err = runTeam(ctx, cfg, *group, *port, *task, mcp)
	} else if *port > 0 {
		err = mcp.RunHTTP(*port)
	} else {
		err = mcp.Run()
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Bind before dispatching the first task so startup failures cannot launch work.
func runTeam(ctx context.Context, cfg Config, group string, port int, task string, mcp *MCPServer) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	loop := NewLoop(cfg, group)
	loop.SetLogFunc(mcp.logFunc)
	loop.SetDoneCh(mcp.DoneCh())
	loop.SetTaskCh(mcp.TaskCh())
	mcp.SetStateFunc(func() string { return loop.State().String() })
	mcp.SetPauseFunc(loop.Pause)
	mcp.SetResumeFunc(loop.Resume)
	server := &http.Server{Handler: mcp.handler(), ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- server.Serve(listener) }()
	go func() { errorsCh <- loop.Run(ctx, task) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errorsCh:
		return err
	}
}
