package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// tools enumerates the MCP tools exposed by docker-mcp. The argument schemas
// are intentionally narrow: the only "free" parameter Claude has is which
// pre-approved profile to run. Profile argv and cwd come exclusively from
// the on-disk config.
var tools = []toolDef{
	{
		Name:        "list_profiles",
		Description: "List the build/command profiles this MCP server is configured to run on the host. Each profile is a fixed argv and working directory pre-approved by the operator. Use this to discover what's available before calling start_job.",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
	{
		Name:        "start_job",
		Description: "Start a job for one of the pre-approved profiles. By default returns immediately with a job_id for polling. Set wait=true to block until the job completes and return the full output (simpler but holds the connection open). Subject to per-profile concurrency limits.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"profile": map[string]interface{}{
					"type":        "string",
					"description": "Name of the profile to run (must match one returned by list_profiles).",
				},
				"wait": map[string]interface{}{
					"type":        "boolean",
					"description": "If true, block until the job completes and return output. If false (default), return immediately with job_id for async polling.",
					"default":     false,
				},
			},
			"required": []string{"profile"},
		},
	},
	{
		Name:        "get_job_status",
		Description: "Get the current state of a job: running, exited (with exit code), killed, timedout, or failed. Includes started_at/finished_at and total lines logged.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"job_id": map[string]interface{}{
					"type":        "string",
					"description": "Job id returned by start_job.",
				},
			},
			"required": []string{"job_id"},
		},
	},
	{
		Name:        "tail_job_log",
		Description: "Read log output from a job. Pass since_line=0 (or omit) to get the most recent max_lines. Pass the next_cursor returned by a previous call to read forward incrementally. Works while the job is running and after it has finished. Returns {lines, next_cursor, done}.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"job_id": map[string]interface{}{
					"type":        "string",
					"description": "Job id returned by start_job.",
				},
				"since_line": map[string]interface{}{
					"type":        "integer",
					"description": "1-based line number to start from. 0 means 'last max_lines'.",
					"default":     0,
				},
				"max_lines": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum lines to return (default 200, max 2000).",
					"default":     200,
				},
			},
			"required": []string{"job_id"},
		},
	},
	{
		Name:        "cancel_job",
		Description: "Send SIGTERM to a running job's process group, followed by SIGKILL after a short grace period. Idempotent for finished jobs.",
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"job_id": map[string]interface{}{
					"type":        "string",
					"description": "Job id returned by start_job.",
				},
			},
			"required": []string{"job_id"},
		},
	},
	{
		Name:        "list_jobs",
		Description: "List recent jobs the server has tracked, newest first. Includes both running and finished jobs (jobs are kept in memory for the server's lifetime).",
		InputSchema: map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		},
	},
}

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

func (s *MCPServer) handleToolCall(req *jsonRPCRequest, sw *StreamingResponseWriter) *jsonRPCResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return toolErrorResponse(req.ID, "invalid params")
	}
	s.log("tool: %s", params.Name)

	switch params.Name {
	case "list_profiles":
		return s.toolListProfiles(req.ID)
	case "start_job":
		return s.toolStartJob(req.ID, params.Arguments, sw)
	case "get_job_status":
		return s.toolGetJobStatus(req.ID, params.Arguments)
	case "tail_job_log":
		return s.toolTailJobLog(req.ID, params.Arguments)
	case "cancel_job":
		return s.toolCancelJob(req.ID, params.Arguments)
	case "list_jobs":
		return s.toolListJobs(req.ID)
	default:
		return toolErrorResponse(req.ID, fmt.Sprintf("unknown tool: %s", params.Name))
	}
}

func (s *MCPServer) toolListProfiles(id interface{}) *jsonRPCResponse {
	out := make([]map[string]interface{}, 0, len(s.cfg.Profiles))
	for _, p := range s.cfg.Profiles {
		out = append(out, map[string]interface{}{
			"name":            p.Name,
			"description":     p.Description,
			"cwd":             p.Cwd,
			"argv":            p.Argv,
			"timeout_seconds": p.TimeoutSeconds,
		})
	}
	return toolJSONResponse(id, map[string]interface{}{
		"profiles": out,
		"count":    len(out),
	})
}

func (s *MCPServer) toolStartJob(id interface{}, args json.RawMessage, sw *StreamingResponseWriter) *jsonRPCResponse {
	var p struct {
		Profile string `json:"profile"`
		Wait    bool   `json:"wait"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErrorResponse(id, "invalid arguments: "+err.Error())
	}
	if strings.TrimSpace(p.Profile) == "" {
		return toolErrorResponse(id, "profile is required")
	}
	job, err := s.jobs.Start(p.Profile)
	if err != nil {
		return toolErrorResponse(id, err.Error())
	}
	s.log("started job %s (profile=%s, wait=%v)", job.ID, job.Profile, p.Wait)

	if !p.Wait {
		// Async mode: return immediately
		return toolJSONResponse(id, job.Snapshot())
	}

	// Sync mode with streaming: stream log output as it comes
	if sw != nil {
		s.streamJobOutput(id, job, sw)
		return nil // Response already written
	}

	// Sync mode without streaming writer: wait and return all at once
	<-job.doneCh

	// Get all output lines
	lines, _, _, _ := job.Tail(1, 10000) // Get up to 10k lines from the start

	snapshot := job.Snapshot()
	snapshot["output"] = lines
	snapshot["output_line_count"] = len(lines)

	return toolJSONResponse(id, snapshot)
}

// streamJobOutput waits for a job to complete while sending periodic keepalive
// bytes to prevent HTTP client timeouts. The response is a standard JSON-RPC
// response with output included. Keepalives are whitespace which JSON parsers ignore.
func (s *MCPServer) streamJobOutput(id interface{}, job *Job, sw *StreamingResponseWriter) {
	// Use standard JSON content type - keepalives are whitespace that JSON ignores
	sw.W.Header().Set("Content-Type", "application/json")
	sw.W.Header().Set("X-Content-Type-Options", "nosniff")

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	// Send a keepalive space and flush - JSON parsers ignore leading whitespace
	sendKeepalive := func() {
		sw.W.Write([]byte(" "))
		if sw.Flusher != nil {
			sw.Flusher.Flush()
		}
	}

	// Send initial keepalive to start the response
	sendKeepalive()

	for {
		select {
		case <-job.doneCh:
			// Job finished - send the full JSON-RPC response
			allLines, _, _, _ := job.Tail(1, 10000)
			snapshot := job.Snapshot()
			snapshot["output"] = allLines
			snapshot["output_line_count"] = len(allLines)

			resp := toolJSONResponse(id, snapshot)
			data, _ := json.Marshal(resp)
			sw.W.Write(data)
			if sw.Flusher != nil {
				sw.Flusher.Flush()
			}
			return

		case <-ticker.C:
			// Send keepalive to prevent timeout
			sendKeepalive()
		}
	}
}

func (s *MCPServer) toolGetJobStatus(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErrorResponse(id, "invalid arguments: "+err.Error())
	}
	job := s.jobs.Get(p.JobID)
	if job == nil {
		return toolErrorResponse(id, fmt.Sprintf("unknown job: %s", p.JobID))
	}
	return toolJSONResponse(id, job.Snapshot())
}

func (s *MCPServer) toolTailJobLog(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		JobID     string `json:"job_id"`
		SinceLine int    `json:"since_line"`
		MaxLines  int    `json:"max_lines"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErrorResponse(id, "invalid arguments: "+err.Error())
	}
	job := s.jobs.Get(p.JobID)
	if job == nil {
		return toolErrorResponse(id, fmt.Sprintf("unknown job: %s", p.JobID))
	}
	lines, next, done, state := job.Tail(p.SinceLine, p.MaxLines)
	return toolJSONResponse(id, map[string]interface{}{
		"job_id":      p.JobID,
		"lines":       lines,
		"line_count":  len(lines),
		"next_cursor": next,
		"done":        done,
		"state":       state,
	})
}

func (s *MCPServer) toolCancelJob(id interface{}, args json.RawMessage) *jsonRPCResponse {
	var p struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return toolErrorResponse(id, "invalid arguments: "+err.Error())
	}
	if err := s.jobs.Cancel(p.JobID); err != nil {
		return toolErrorResponse(id, err.Error())
	}
	return toolTextResponse(id, fmt.Sprintf("Cancellation signal sent to job %s.", p.JobID))
}

func (s *MCPServer) toolListJobs(id interface{}) *jsonRPCResponse {
	jobs := s.jobs.List()
	out := make([]map[string]interface{}, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Snapshot())
	}
	return toolJSONResponse(id, map[string]interface{}{
		"jobs":  out,
		"count": len(out),
	})
}
