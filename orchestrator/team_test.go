package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTeamLifecycle(t *testing.T) {
	prompts := make(chan string, 8)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/children"):
			json.NewEncoder(w).Encode([]InstanceInfo{{Title: "leader", AgentPreset: "orchestrator", Status: "ready"}, {Title: "worker", Status: "ready"}})
		case strings.HasSuffix(r.URL.Path, "/send"):
			var b map[string]string
			json.NewDecoder(r.Body).Decode(&b)
			prompts <- r.URL.Path + ":" + b["text"]
			w.Write([]byte(`{}`))
		default:
			w.Write([]byte(`{"events":[]}`))
		}
	}))
	defer api.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	cfg := DefaultConfig()
	cfg.BaseURL = api.URL
	cfg.PollInterval = 10 * time.Millisecond
	mcp := NewMCPServer(api.URL, "team")
	mcp.SetLogFunc(func(string) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runTeam(ctx, cfg, "team", port, "test task", mcp) }()
	expectPrompt := func(want string) {
		t.Helper()
		select {
		case p := <-prompts:
			if !strings.Contains(p, want) {
				t.Fatal(p)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("missing prompt %s", want)
		}
	}
	expectPrompt("test task")
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	post := func(path, body string) {
		t.Helper()
		r, e := http.Post(base+path, "application/json", strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d", path, r.StatusCode)
		}
	}
	state := func(want string) {
		t.Helper()
		for i := 0; i < 100; i++ {
			r, e := http.Get(base + "/status")
			if e == nil {
				var b map[string]interface{}
				json.NewDecoder(r.Body).Decode(&b)
				r.Body.Close()
				if b["state"] == want {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("missing state %s", want)
	}
	state("running")
	post("/pause", `{}`)
	state("paused")
	post("/resume", `{}`)
	state("running")
	post("/", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_to_agent","arguments":{"agent":"worker","prompt":"work"}}}`)
	expectPrompt("/worker/send:work")
	post("/pause", `{}`)
	post("/", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"mark_task_done","arguments":{"summary":"finished"}}}`)
	state("done")
	post("/task", `{"task":"second task"}`)
	expectPrompt("second task")
	state("running")
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
}
func TestManagedCompletion(t *testing.T) {
	var path string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { path = r.URL.Path; w.Write([]byte(`{}`)) }))
	defer api.Close()
	mcp := NewMCPServer(api.URL, "my-team")
	mcp.managed = true
	if _, e := mcp.toolMarkTaskDone(json.RawMessage(`{"summary":"finished"}`)); e != nil {
		t.Fatal(e)
	}
	if path != "/api/instances/my-team/orchestrator/complete" {
		t.Fatal(path)
	}
	select {
	case <-mcp.DoneCh():
		t.Fatal("completion stranded in standalone MCP")
	default:
	}
}
func TestTeamBindFailure(t *testing.T) {
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer ln.Close()
	mcp := NewMCPServer("http://127.0.0.1:1", "team")
	if e := runTeam(context.Background(), DefaultConfig(), "team", ln.Addr().(*net.TCPAddr).Port, "task", mcp); e == nil {
		t.Fatal("expected bind error")
	}
}

func TestToolsRejectNonMembers(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/children") {
			t.Errorf("unexpected request to %s", r.URL.Path)
		}
		w.Write([]byte(`[{"title":"worker"}]`))
	}))
	defer api.Close()
	mcp := NewMCPServer(api.URL, "team")
	args := json.RawMessage(`{"agent":"other-team-worker","prompt":"work"}`)
	if _, err := mcp.toolSendToAgent(args); err == nil {
		t.Fatal("sent outside team")
	}
	if _, err := mcp.toolReadAgentOutput(args); err == nil {
		t.Fatal("read outside team")
	}
	if _, err := mcp.toolGetAgentStatus(args); err == nil {
		t.Fatal("queried outside team")
	}
}

func TestReadAgentOutputPreservesFullMessages(t *testing.T) {
	for _, kind := range []string{"assistant_text", "thinking", "tool_result", "error"} {
		t.Run(kind, func(t *testing.T) {
			full := strings.Repeat("Long message 内容\n", 500) + "END OF MESSAGE"
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/children") {
					json.NewEncoder(w).Encode([]InstanceInfo{{Title: "worker"}})
					return
				}
				if r.URL.Query().Get("tail") != "2" || r.URL.Query().Get("offset") != "3" {
					t.Error("pagination parameters were not preserved")
				}
				json.NewEncoder(w).Encode(HistoryResponse{
					Events:     []Event{{Type: kind, Text: full, Output: full}, {Type: "tool_result", Output: full, IsError: true}},
					TotalCount: 10, HasMore: true,
				})
			}))
			defer api.Close()
			mcp := NewMCPServer(api.URL, "team")
			output, err := mcp.toolReadAgentOutput(json.RawMessage(`{"agent":"worker","count":2,"offset":3}`))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(output, full) != 2 {
				t.Fatal("message or error output was truncated")
			}
			if !strings.Contains(output, "has_more: true") {
				t.Fatal("missing pagination information")
			}
		})
	}
}
