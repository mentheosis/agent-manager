package taskqueues

import (
	"encoding/json"
	"testing"
)

func TestTaskExplanationAuthority(t *testing.T) {
	worker := json.RawMessage(`{"summary":"Old worker blocker","handoff":{"headline":"Old blocker","category":"engineering"}}`)
	review := json.RawMessage(`{"decision":"escalate","summary":"Waiting for access","external_dependency":"Provider cooldown","next_steps":["Retry tomorrow"],"handoff":{"headline":"Waiting for source access","category":"waiting","next_action":"Retry tomorrow"}}`)
	task := Task{Status: "blocked", LatestResult: worker, LatestError: "Waiting for access", Rounds: []Round{{Role: "reviewer", Submission: review}}}
	x := explainTask(task)
	if x.Source != "reviewer" || x.Category != "waiting" {
		t.Fatalf("reviewer did not win: %+v", x)
	}
	task.LatestError = "Execution infrastructure failed: argument list too long"
	x = explainTask(task)
	if x.Source != "controller" || x.Category != "system_error" || x.Completed != "" {
		t.Fatalf("stale review masks error: %+v", x)
	}
	task.LatestError = "Token budget exhausted: 100 used"
	x = explainTask(task)
	if x.Category != "limit" {
		t.Fatal(x)
	}
	task.Status = "running"
	task.Rounds = []Round{{Role: "reviewer"}}
	x = explainTask(task)
	if x.Headline != "Awaiting review" || x.Reason != "" {
		t.Fatal(x)
	}
	task.Status = "queued"
	x = explainTask(task)
	if x.Headline != "Waiting to start" || x.Category != "none" {
		t.Fatal(x)
	}
}
func TestHandoffValidation(t *testing.T) {
	var old *Handoff
	if err := old.validate(); err != nil {
		t.Fatal(err)
	}
	h := &Handoff{Headline: "Waiting", Completed: "Producer works", Remaining: "Real sample", Reason: "Rate limited", NextAction: "Retry", Actor: "operator", Readiness: "Not ready", Category: "waiting", RetryAfter: "2026-09-26T07:55:56Z"}
	if err := h.validate(); err != nil {
		t.Fatal(err)
	}
	h.RetryAfter = "tomorrow"
	if h.validate() == nil {
		t.Fatal("accepted ambiguous timestamp")
	}
	h.RetryAfter = ""
	h.NextAction = ""
	if h.validate() == nil {
		t.Fatal("accepted missing action")
	}
}
