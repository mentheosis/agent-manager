package taskqueues

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	orchestration "github.com/anthropics/agent-manager/orchestrator"
)

func Run(ctx context.Context, c Config, port int, log func(string)) error {
	store, e := Open(c)
	if e != nil {
		return e
	}
	defer store.DB.Close()
	c = store.Config
	if c.QueueID == "" {
		return errors.New("queue identifier is required")
	}
	lock, e := store.controls.lockProcess()
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = store.Check(ctx); e != nil {
		return e
	}
	scheduler := &Scheduler{Store: store, Workers: &APIWorkers{orchestration.NewClient(c.BaseURL), c}, Owner: c.ControllerID, Log: log}
	mux := http.NewServeMux()
	var operations sync.Mutex
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		status, e := store.Status(r.Context())
		if e != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		state := "running"
		if status.Paused {
			state = "paused"
		}
		writeJSON(w, map[string]any{"state": state, "group": c.Parent, "pid": os.Getpid(), "queue": status})
	})
	mux.HandleFunc("/tasks", func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if offset < 0 {
			offset = 0
		}
		tasks, e := store.Tasks(r.Context(), offset)
		respond(w, tasks, e)
	})
	mux.HandleFunc("/logs", func(w http.ResponseWriter, r *http.Request) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		logs, e := store.Logs(r.Context(), after)
		respond(w, logs, e)
	})
	mux.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			Action  string `json:"action"`
			Turn    string `json:"turn_id"`
			Text    string `json:"text"`
			Limits  Limits `json:"limits"`
			Max     *int   `json:"max_workers"`
			Task    int64  `json:"task_id"`
			Attempt string `json:"attempt_id"`
			Actor   string `json:"actor"`
		}
		if !decode(w, r, &body) {
			return
		}
		operations.Lock()
		defer operations.Unlock()
		var e error
		switch body.Action {
		case "pause", "resume", "max_workers":
			var paused *bool
			if body.Action != "max_workers" {
				p := body.Action == "pause"
				paused = &p
			}
			e = store.Configure(r.Context(), body.Max, paused, body.Actor)
		case "limits":
			e = store.ConfigureLimits(r.Context(), body.Limits, body.Actor)
		case "cancel_all":
			var attempts []Attempt
			attempts, e = store.Attempts(r.Context())
			if e == nil {
				for _, a := range attempts {
					if a.Owner == c.ControllerID {
						if e = scheduler.CancelTask(r.Context(), a.TaskID, body.Actor); e != nil {
							break
						}
					}
				}
			}
		case "conversation_prompt":
			var managed bool
			managed, e = scheduler.PromptTask(r.Context(), body.Task, body.Attempt, body.Actor, ConversationPrompt{body.Turn, body.Text})
			respond(w, map[string]bool{"ok": e == nil, "managed": managed}, e)
			return
		case "resume_attempt":
			e = scheduler.ResumeTask(r.Context(), body.Task, body.Attempt, body.Actor)
		case "pause_attempt":
			e = scheduler.PauseConversation(r.Context(), body.Task, body.Attempt, body.Turn, body.Actor)
		case "cancel":
			e = scheduler.CancelTask(r.Context(), body.Task, body.Actor, body.Attempt)
		default:
			e = store.Action(r.Context(), body.Task, body.Action, body.Attempt, body.Actor)
		}
		respond(w, map[string]bool{"ok": e == nil}, e)
	})
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		var body struct {
			Attempt string          `json:"attempt_id"`
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		if !decode(w, r, &body) {
			return
		}
		e := store.Submit(r.Context(), body.Attempt, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), body.Kind, body.Payload)
		respond(w, map[string]bool{"ok": e == nil}, e)
	})
	// All control/query routes require the supervisor token. Worker submissions have
	// their own per-attempt capability and are checked transactionally by Submit.
	secured := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/submit" && r.URL.Path != "/status" && r.Header.Get("Authorization") != "Bearer "+c.InternalToken {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return orchestration.Serve(ctx, port, secured, func(ctx context.Context) error { return scheduler.Run(ctx, &operations) })
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if e := json.NewDecoder(r.Body).Decode(v); e != nil {
		http.Error(w, "invalid request", 400)
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
func respond(w http.ResponseWriter, v any, e error) {
	if e != nil {
		code := 400
		if errors.Is(e, ErrConflict) {
			code = 409
		}
		http.Error(w, e.Error(), code)
		return
	}
	writeJSON(w, v)
}
