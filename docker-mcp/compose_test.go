package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestComposeRequiresTypedRequest(t *testing.T) {
	c := &Config{Profiles: []Profile{{Name: "compose", Compose: &ComposeConfig{}}}}
	m := &JobManager{cfg: c}
	if _, err := m.Start("compose"); err == nil {
		t.Fatal("untyped start permitted")
	}
	s := NewMCPServer(c, nil)
	for _, raw := range []string{`{"profile":"compose","action":"shell"}`, `{"profile":"missing","action":"start"}`, `{"profile":"compose","action":"start","argv":["sh"]}`} {
		response := s.toolCompose(1, json.RawMessage(raw))
		data, _ := json.Marshal(response)
		if !strings.Contains(string(data), `"isError":true`) {
			t.Fatal("invalid request accepted")
		}
	}
}
