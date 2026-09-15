package main

import (
	"bytes"
	"encoding/json"
	"io"
)

type ComposeConfig struct {
	Docker           string              `json:"docker"`
	ComposeFiles     []string            `json:"compose_files"`
	Service          string              `json:"service"`
	StateDir         string              `json:"state_dir"`
	ConfigFile       string              `json:"config_file"`
	ContainerConfig  string              `json:"container_config"`
	Operations       map[string][]string `json:"operations"`
	CommandPrefix    []string            `json:"command_prefix"`
	ContainerPython  string              `json:"container_python,omitempty"`
	SourceExtensions []string            `json:"source_extensions,omitempty"`
	SourcePaths      []string            `json:"source_paths,omitempty"`
	Database         *ComposeDatabase    `json:"database,omitempty"`
	TimeoutSeconds   int                 `json:"timeout_seconds"`
}

type ComposeDatabase struct {
	ConfigKeys    []string `json:"config_keys"`
	Host          string   `json:"host"`
	Name          string   `json:"name"`
	AllowedTables []string `json:"allowed_tables"`
}

func init() {
	tools = append(tools, toolDef{Name: "compose", Description: "Run or inspect a host-approved Compose operation. start is idempotent for run_key; persist that key and reuse it after interruption. status/logs/cancel survive MCP restart. query is bounded read-only SQL. Returns a bridge job ID; get_job_status/tail_job_log return the operation result. Cancelling the bridge job does not cancel the container: use compose action cancel.", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"profile": map[string]interface{}{"type": "string"}, "action": map[string]interface{}{"type": "string", "enum": []string{"start", "status", "logs", "cancel", "query"}}, "run_key": map[string]interface{}{"type": "string", "maxLength": 80}, "operation": map[string]interface{}{"type": "string"}, "sql": map[string]interface{}{"type": "string", "maxLength": 16000}}, "required": []string{"profile", "action"}}})
}
func (s *MCPServer) toolCompose(id interface{}, raw json.RawMessage) *jsonRPCResponse {
	var args struct {
		Profile   string `json:"profile"`
		Action    string `json:"action"`
		RunKey    string `json:"run_key"`
		Operation string `json:"operation"`
		SQL       string `json:"sql"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&args) != nil || d.Decode(new(interface{})) != io.EOF {
		return toolErrorResponse(id, "invalid Compose arguments")
	}
	p := s.cfg.FindProfile(args.Profile)
	if p == nil || p.Compose == nil {
		return toolErrorResponse(id, "unknown Compose profile")
	}
	if args.Action != "start" && args.Action != "status" && args.Action != "logs" && args.Action != "cancel" && args.Action != "query" {
		return toolErrorResponse(id, "invalid Compose action")
	}
	input, _ := json.Marshal(map[string]interface{}{"config": p.Compose, "cwd": p.Cwd, "action": args.Action, "run_key": args.RunKey, "operation": args.Operation, "sql": args.SQL})
	job, err := s.jobs.start(args.Profile, input)
	if err != nil {
		return toolErrorResponse(id, err.Error())
	}
	return toolJSONResponse(id, job.Snapshot())
}
