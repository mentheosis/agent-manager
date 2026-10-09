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
	role := os.Getenv("AM_QUEUE_ROLE")
	files := map[string]string{}
	if raw := os.Getenv("AM_REVIEW_FILES"); raw != "" {
		if e := json.Unmarshal([]byte(raw), &files); e != nil {
			return e
		}
	}
	tools := []any{
		map[string]any{"name": "queue_athena", "description": "Query the approved read-only Athena audit profile. action=query requires sql and optional max_rows (1–1000). Poll action=job_status with job_id, then action=job_logs with job_id and optional since_line (start at 1; follow next_cursor). Only this worker's jobs can be read. No replay profile is required.", "inputSchema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"action": map[string]any{"type": "string", "enum": []string{"query", "job_status", "job_logs"}}, "sql": map[string]any{"type": "string", "maxLength": 65536}, "max_rows": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}, "job_id": map[string]any{"type": "string"}, "since_line": map[string]any{"type": "integer", "minimum": 1}}, "required": []string{"action"}}},
		map[string]any{"name": "queue_read_file", "description": "Reviewer: omit path to list approved input and proposal files. Read by exact listed key. Follow next_offset while has_more; no shell or host MCP is required.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}, "offset": map[string]string{"type": "integer"}}}},
		map[string]any{"name": "queue_read_history", "description": "Read intact conversation events from another turn in this attempt. Use round IDs or conversation IDs in review history; paginate using next_offset while has_more is true.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"turn_id": map[string]string{"type": "string"}, "offset": map[string]string{"type": "integer"}}}},
		map[string]any{"name": "queue_submit_review", "description": "Reviewer only: accept evidenced completion, continue with concrete investigations, or escalate an external dependency. Stop after successful submission.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"decision": map[string]any{"type": "string", "enum": []string{"continue", "accept", "escalate"}}, "summary": map[string]string{"type": "string"}, "evidence": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}, "next_steps": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}, "external_dependency": map[string]string{"type": "string"}}, "required": []string{"decision", "summary", "evidence"}}},
		map[string]any{"name": "queue_replay", "description": "Use the approved replay profile. start/status/logs/cancel/query call the host runner; job_status/job_logs poll the returned bridge job. run_key is a short scenario-step suffix automatically scoped to this queue task across retries.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"action": map[string]string{"type": "string"}, "run_key": map[string]string{"type": "string"}, "operation": map[string]string{"type": "string"}, "sql": map[string]string{"type": "string"}, "job_id": map[string]string{"type": "string"}}, "required": []string{"action"}}},
		map[string]any{"name": "queue_progress", "description": "Record concise progress, evidence or a finding for this attempt.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]string{"type": "string"}}, "required": []string{"summary"}}},
		map[string]any{"name": "queue_submit_result", "description": "Submit the final structured outcome. Artifact paths must be relative to your workspace; immutable copies are retained. Stop work after successful submission.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"outcome": map[string]any{"type": "string", "enum": []string{"completed", "blocked", "failed"}}, "summary": map[string]string{"type": "string"}, "artifact_paths": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}, "blockers": map[string]any{"type": "array", "items": map[string]string{"type": "string"}}}, "required": []string{"outcome", "summary"}}},
	}
	// Require a readable handoff on new worker/reviewer submissions.
	for _, entry := range tools {
		tool := entry.(map[string]any)
		if tool["name"] != "queue_submit_result" && tool["name"] != "queue_submit_review" {
			continue
		}
		schema := tool["inputSchema"].(map[string]any)
		props := map[string]any{}
		for _, key := range []string{"headline", "completed", "remaining", "reason", "next_action", "actor", "readiness", "retry_after"} {
			props[key] = map[string]any{"type": "string"}
		}
		props["category"] = map[string]any{"type": "string", "enum": []string{"none", "human_input", "waiting", "engineering", "system_error", "limit"}}
		schema["properties"].(map[string]any)["handoff"] = map[string]any{"type": "object", "properties": props, "required": []string{"headline", "completed", "remaining", "reason", "next_action", "actor", "category", "readiness"}, "description": "Standalone plain-language explanation for the human. State what works, what remains, why work stopped, who acts and the precise next action. Explain technical terms; no unexplained IDs. Use none explicitly where appropriate. retry_after is an optional RFC3339 timestamp, not a promise of automatic resumption. Repeat this explanation in your visible final response and main report. Reviewers must correct unclear handoffs; unfinished feasible engineering requires continue."}
		schema["required"] = append(schema["required"].([]string), "handoff")
	}
	allowed := map[string]bool{"queue_progress": true, "queue_read_history": true}
	if role == "reviewer" {
		allowed["queue_read_file"] = true
		allowed["queue_submit_review"] = true
	} else {
		allowed["queue_submit_result"] = true
		allowed["queue_replay"] = true
		allowed["queue_athena"] = true
	}
	filtered := []any{}
	for _, entry := range tools {
		tool := entry.(map[string]any)
		name := tool["name"].(string)
		if !allowed[name] {
			continue
		}
		tool["annotations"] = map[string]any{"readOnlyHint": name == "queue_read_file" || name == "queue_read_history" || name == "queue_athena", "destructiveHint": false, "openWorldHint": name == "queue_replay" || name == "queue_athena"}
		filtered = append(filtered, tool)
	}
	tools = filtered
	client := orchestration.NewClient(base)
	return orchestration.ServeMCP(os.Stdin, os.Stdout, tools, func(name string, args json.RawMessage) (any, error) {
		if name == "queue_submit_result" || name == "queue_submit_review" {
			var request struct {
				Handoff *Handoff `json:"handoff"`
			}
			if err := json.Unmarshal(args, &request); err != nil {
				return nil, err
			}
			if request.Handoff == nil {
				return nil, errors.New("handoff is required: explain completed work, remaining work, reason, next action, actor and readiness for the human")
			}
			if err := request.Handoff.validate(); err != nil {
				return nil, err
			}
		}
		if !allowed[name] {
			return nil, errors.New("tool is not enabled for this role")
		}
		if name == "queue_read_file" {
			return readReviewFile(files, args)
		}
		if name == "queue_read_history" {
			var out any
			err := client.Request(context.Background(), http.MethodPost, "/api/task-queues/"+url.PathEscape(parent)+"/attempts/"+url.PathEscape(attempt)+"/history", args, &out, token)
			return out, err
		}
		if name == "queue_replay" || name == "queue_athena" {
			path := "replay"
			if name == "queue_athena" {
				path = "athena"
			}
			var out any
			err := client.Request(context.Background(), http.MethodPost, "/api/task-queues/"+url.PathEscape(parent)+"/attempts/"+url.PathEscape(attempt)+"/"+path, args, &out, token)
			return out, err
		}
		kind := ""
		if name == "queue_progress" {
			kind = "progress"
		}
		if name == "queue_submit_result" {
			kind = "result"
		}
		if name == "queue_submit_review" {
			kind = "review"
		}
		if kind == "" {
			return nil, errors.New("unknown tool")
		}
		var out any
		err := client.Request(context.Background(), http.MethodPost, "/api/task-queues/"+url.PathEscape(parent)+"/submit", map[string]any{"attempt_id": attempt, "kind": kind, "payload": args}, &out, token)
		return out, err
	})
}
