package main

import (
	"bytes"
	"encoding/json"
	"io"
)

func init() {
	tools = append(tools, toolDef{Name: "athena_query", Description: "Start one read-only Athena query on a host-configured profile. Returns a job ID; use get_job_status and tail_job_log for bounded JSON results. Results may be truncated.", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"profile":  map[string]interface{}{"type": "string"},
			"sql":      map[string]interface{}{"type": "string", "maxLength": 65536},
			"max_rows": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": 1000},
		}, "required": []string{"profile", "sql"},
	}})
}

func (s *MCPServer) toolAthenaQuery(id interface{}, raw json.RawMessage) *jsonRPCResponse {
	var args struct {
		Profile string `json:"profile"`
		SQL     string `json:"sql"`
		MaxRows int    `json:"max_rows"`
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil {
		return toolErrorResponse(id, "invalid Athena arguments: "+err.Error())
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return toolErrorResponse(id, "unexpected trailing input")
	}
	p := s.cfg.FindProfile(args.Profile)
	if p == nil || p.Athena == nil {
		return toolErrorResponse(id, "unknown Athena profile")
	}
	if len(args.SQL) == 0 || len(args.SQL) > 65536 {
		return toolErrorResponse(id, "sql must be 1..65536 bytes")
	}
	if args.MaxRows == 0 {
		args.MaxRows = 1000
	}
	if args.MaxRows < 1 || args.MaxRows > 1000 {
		return toolErrorResponse(id, "max_rows must be 1..1000")
	}
	input, err := json.Marshal(map[string]interface{}{"config": p.Athena, "sql": args.SQL, "max_rows": args.MaxRows, "timeout_seconds": p.TimeoutSeconds - 10})
	if err != nil {
		return toolErrorResponse(id, err.Error())
	}
	job, err := s.jobs.start(args.Profile, input)
	if err != nil {
		return toolErrorResponse(id, err.Error())
	}
	return toolJSONResponse(id, job.Snapshot())
}
