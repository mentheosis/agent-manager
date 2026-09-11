package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAthenaArguments(t *testing.T) {
	s := NewMCPServer(&Config{Profiles: []Profile{{Name: "audit", Athena: &AthenaConfig{}}}}, nil)
	for _, input := range []string{
		`{"profile":"audit","sql":"SELECT 1","argv":["sh"]}`,
		`{"profile":"audit","sql":"SELECT 1","max_rows":1001}`,
		`{"profile":"missing","sql":"SELECT 1"}`,
		`{"profile":"audit","sql":""}`,
	} {
		r := s.toolAthenaQuery(1, json.RawMessage(input))
		b, _ := json.Marshal(r)
		if !strings.Contains(string(b), `"isError":true`) {
			t.Fatalf("expected rejection: %s", b)
		}
	}
}

func TestAthenaCannotUseStartJob(t *testing.T) {
	m := &JobManager{cfg: &Config{Profiles: []Profile{{Name: "audit", Athena: &AthenaConfig{}}}}}
	if _, err := m.Start("audit"); err == nil {
		t.Fatal("expected typed invocation requirement")
	}
}

func TestHTTPBodyLimitAndAuth(t *testing.T) {
	h := NewHTTPServer(nil, "secret")
	handler := h.requireAuth(h.handleRPC)
	r := httptest.NewRequest("POST", "/", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handler(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	r = httptest.NewRequest("POST", "/", strings.NewReader(strings.Repeat("x", 129*1024)))
	r.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	handler(w, r)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
