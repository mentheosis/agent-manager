package taskqueues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Exercise the real queue HTTP server and API worker adapter. The provider is a
// deterministic HTTP fixture: no paid session, external writes or real task data.
func TestHTTPQueueControllerNeutralTask(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg, task := definitionFixture(t)
	s.Config.Repositories = cfg.Repositories
	s.Config.Tasks = cfg.Tasks
	s.Config.ControllerID = "test-controller"
	s.Config.Parent = "queue"
	id := enqueue(t, s, true, nil)
	if _, err := s.DB.ExecContext(ctx, "UPDATE am_tasks SET parameters=? WHERE id=?", string(task.Parameters), id); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 2 * time.Second}
	request := func(method, path, token string, body any) ([]byte, int, error) {
		raw, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(raw))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		return data, response.StatusCode, err
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+s.Config.InternalToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		if r.Method == "POST" {
			var body struct {
				Workspace string `json:"workspace"`
				Prompt    string `json:"prompt"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				http.Error(w, "bad request", 400)
				return
			}
			if !strings.Contains(body.Prompt, "report about example") {
				t.Error("worker did not receive resolved instructions")
			}
			if err := os.WriteFile(body.Workspace+"/report.md", []byte("Neutral HTTP proof"), 0600); err != nil {
				t.Error(err)
			}
			attempt := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			for _, kind := range []string{"progress", "result"} {
				payload := map[string]any{"summary": "Neutral report complete"}
				if kind == "result" {
					payload["outcome"] = "completed"
					payload["artifact_paths"] = []string{"report.md"}
				}
				data, status, err := request("POST", "/submit", capability(s.Config.InternalToken, attempt), map[string]any{"attempt_id": attempt, "kind": kind, "payload": payload})
				if err != nil || status != 200 {
					t.Errorf("submit: %s %d %v", data, status, err)
				}
			}
		}
		json.NewEncoder(w).Encode(WorkerState{Exists: true, Alive: false, Launched: true, Conversation: "neutral-conversation"})
	}))
	defer backend.Close()
	s.Config.BaseURL = backend.URL
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s.Config, port, func(string) {}) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("controller failed to stop")
		}
	}()
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, status, err := request("GET", "/status", "", nil); err == nil && status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("controller did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, status, _ := request("POST", "/control", "wrong", map[string]any{"action": "resume"}); status != 401 {
		t.Fatal("unauthenticated control", status)
	}
	if data, status, err := request("POST", "/control", s.Config.InternalToken, map[string]any{"action": "resume", "actor": "test"}); err != nil || status != 200 {
		t.Fatal(string(data), status, err)
	}
	var got Task
	for {
		got, err = s.Task(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "awaiting_review" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not reach review: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if data, status, err := request("POST", "/control", s.Config.InternalToken, map[string]any{"action": "approve", "actor": "test", "task_id": id, "attempt_id": got.Latest}); err != nil || status != 200 {
		t.Fatal(string(data), status, err)
	}
	got, err = s.Task(ctx, id)
	if err != nil || got.Status != "completed" || !strings.Contains(string(got.Result), "sha256") {
		t.Fatal(got, err)
	}
}
