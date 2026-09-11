package taskqueues

import (
	"context"
	"encoding/json"
	"errors"
	orchestration "github.com/anthropics/agent-manager/orchestrator"
	"net/http"
	"net/url"
	"os"
)

func Worker(base, parent, attempt string) error {
	token := os.Getenv("AM_ATTEMPT_TOKEN")
	if token == "" || attempt == "" {
		return errors.New("attempt capability is missing")
	}
	tools := []any{
		map[string]any{"name": "queue_progress", "description": "Record concise progress, evidence or a finding for this attempt.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]string{"type": "string"}}, "required": []string{"summary"}}},
		map[string]any{"name": "queue_submit_result", "description": "Submit the final structured outcome. Artifact paths must be relative to your workspace; immutable copies are retained. Stop work after successful submission.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"outcome": map[string]any{"type": "string", "enum": []string{"completed", "blocked", "failed"}}, "summary": map[string]string{"type": "string"}, "artifact_paths": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}, "blockers": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}}, "required": []string{"outcome", "summary"}}},
	}
	client := orchestration.NewClient(base)
	return orchestration.ServeMCP(os.Stdin, os.Stdout, tools, func(name string, args json.RawMessage) (any, error) {
		kind := ""
		if name == "queue_progress" {
			kind = "progress"
		}
		if name == "queue_submit_result" {
			kind = "result"
		}
		if kind == "" {
			return nil, errors.New("unknown tool")
		}
		var out any
		err := client.Request(context.Background(), http.MethodPost, "/api/task-queues/"+url.PathEscape(parent)+"/submit", map[string]any{"attempt_id": attempt, "kind": kind, "payload": args}, &out, token)
		return out, err
	})
}
