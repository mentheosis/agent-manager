package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	taskqueues "github.com/anthropics/agent-manager/orchestrator/task_queues"
	"github.com/anthropics/agent-manager/orchestrator/teams"

	"os"
	"os/signal"
	"syscall"
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:8787", "agent-manager API base URL")
	group := flag.String("group", "", "parent team title (required)")
	mode := flag.String("mode", "mcp", "team control loop or standalone mcp tools")
	port := flag.Int("mcp-port", 0, "localhost HTTP port (required for team mode; 0 uses stdio in mcp mode)")
	task := flag.String("task", "", "initial team task")
	managed := flag.Bool("managed", false, "forward MCP completion to the managed team controller")
	attempt := flag.String("attempt", "", "queue worker attempt ID")
	initSchema := flag.Bool("init-schema", false, "explicitly initialize generic queue schema")
	queueAction := flag.String("queue-action", "", "render or enqueue a task batch from stdin")
	flag.Parse()
	if *mode == "queue-worker" {
		if err := taskqueues.Worker(*baseURL, *group, *attempt); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *mode == "task-queue" {
		cfg, err := taskqueues.LoadConfig()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		cfg.BaseURL = *baseURL
		cfg.Parent = *group
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if *queueAction != "" {
			err = taskqueues.QueueCommand(ctx, cfg, *queueAction, os.Stdin, os.Stdout)
		} else if *initSchema {
			var store *taskqueues.Store
			store, err = taskqueues.Open(cfg)
			if err == nil {
				defer store.DB.Close()
				err = store.Init(ctx)
			}
		} else if *port <= 0 || *group == "" {
			err = fmt.Errorf("task-queue requires --group and --mcp-port")
		} else {
			err = taskqueues.Run(ctx, cfg, *port, func(s string) { fmt.Fprintln(os.Stderr, s) })
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "Queue controller failed:", err)
			os.Exit(1)
		}
		return
	}
	if *group == "" || (*mode != "team" && *mode != "mcp") || (*mode == "team" && *port <= 0) {
		fmt.Fprintln(os.Stderr, "Require --group and --mode mcp|team; team mode requires --mcp-port")
		os.Exit(1)
	}
	mcp := teams.NewMCPServer(*baseURL, *group)
	mcp.SetManaged(*managed)
	log := func(s string) { fmt.Fprintln(os.Stderr, s) }
	mcp.SetLogFunc(log)
	var err error
	if *mode == "team" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		cfg := teams.DefaultConfig()
		cfg.BaseURL = *baseURL
		err = teams.Run(ctx, cfg, *group, *port, *task, mcp)
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
