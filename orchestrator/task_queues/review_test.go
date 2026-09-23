package taskqueues

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestReviewDecisionContract(t *testing.T) {
	for _, tc := range []struct {
		d  ReviewDecision
		p  Submission
		ok bool
	}{
		{ReviewDecision{Decision: "accept", Summary: "Verified", Evidence: []string{"fixture 3"}}, Submission{Outcome: "completed"}, true},
		{ReviewDecision{Decision: "accept", Summary: "Verified", Evidence: []string{"fixture 3"}}, Submission{Outcome: "blocked"}, false},
		{ReviewDecision{Decision: "continue", Summary: "More research", Evidence: []string{"missing historical sample"}}, Submission{}, false},
		{ReviewDecision{Decision: "continue", Summary: "More research", Evidence: []string{"missing historical sample"}, NextSteps: []string{"Query block 100"}}, Submission{}, true},
		{ReviewDecision{Decision: "escalate", Summary: "Need access", Evidence: []string{"HTTP 403"}}, Submission{}, false},
	} {
		if e := tc.d.validate(tc.p); (e == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc, e)
		}
	}
}
func TestReviewRoundsContinueAcceptAndHumanGate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, task := definitionFixture(t)
	s.Config.Repositories = c.Repositories
	s.Config.Tasks = c.Tasks
	def := s.Config.Tasks["neutral"]
	def.Reviewer = &Reviewer{Provider: "codex", Permission: "read-only", Model: "test", Instructions: def.Instructions}
	s.Config.Tasks["neutral"] = def
	id := enqueue(t, s, true, nil)
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_tasks SET parameters=? WHERE id=?", string(task.Parameters), id); e != nil {
		t.Fatal(e)
	}
	resume(t, s, 1)
	w := &fakeWorkers{states: map[string]WorkerState{}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	tick := func() {
		t.Helper()
		if e := scheduler.Tick(ctx); e != nil {
			t.Fatal(e)
		}
	}
	tick()
	attempts, _ := s.Attempts(ctx)
	a := attempts[0]
	tick()
	tick()
	rounds, _ := s.Rounds(ctx, a.ID)
	if len(rounds) != 1 || w.launches != 1 {
		t.Fatal(rounds, w.launches)
	}
	// Restart reconciliation must not duplicate the already reserved turn.
	scheduler = Scheduler{s, w, "owner", func(string) {}}
	tick()
	if w.launches != 1 {
		t.Fatal("duplicate launch")
	}
	submit := func(r Round, kind string, p any) {
		t.Helper()
		raw, _ := json.Marshal(p)
		if e := s.Submit(ctx, r.ID, capability(s.Config.InternalToken, r.ID), kind, raw); e != nil {
			t.Fatal(e)
		}
		state := w.states[r.ID]
		state.Busy = false
		w.states[r.ID] = state
	}
	submit(rounds[0], "result", Submission{Outcome: "blocked", Summary: "Historical query remains to be tried"})
	tick()
	tick()
	rounds, _ = s.Rounds(ctx, a.ID)
	if len(rounds) != 2 || rounds[1].Role != "reviewer" {
		t.Fatal(rounds)
	}
	submit(rounds[1], "review", ReviewDecision{Decision: "continue", Summary: "An authorized experiment remains", Evidence: []string{"worker proposal"}, NextSteps: []string{"Run the historical query"}})
	tick()
	tick()
	rounds, _ = s.Rounds(ctx, a.ID)
	if len(rounds) != 3 || rounds[2].Number != 2 {
		t.Fatal(rounds)
	}
	feedback, _ := json.MarshalIndent(ReviewDecision{Decision: "continue", Summary: "An authorized experiment remains", Evidence: []string{"worker proposal"}, NextSteps: []string{"Run the historical query"}}, "", "  ")
	if rounds[2].Prompt != "Reviewer feedback:\n"+string(feedback) {
		t.Fatalf("continuation must contain only the latest reviewer feedback: %s", rounds[2].Prompt)
	}
	raw, _ := json.Marshal(Submission{Outcome: "completed", Summary: "stale"})
	if e := s.Submit(ctx, rounds[0].ID, capability(s.Config.InternalToken, rounds[0].ID), "result", raw); e == nil {
		t.Fatal("stale result accepted")
	}
	if e := os.WriteFile(a.Workspace+"/report.md", []byte("Evidence for reconstruction"), 0600); e != nil {
		t.Fatal(e)
	}
	submit(rounds[2], "result", Submission{Outcome: "completed", Summary: "Verified", ArtifactPaths: []string{"report.md"}})
	tick()
	tick()
	rounds, _ = s.Rounds(ctx, a.ID)
	submit(rounds[3], "review", ReviewDecision{Decision: "accept", Summary: "Evidence satisfies criteria", Evidence: []string{"report.md"}})
	tick()
	got, e := s.Task(ctx, id)
	if e != nil || got.Status != "awaiting_review" || got.AttemptCount != 1 {
		t.Fatal(got, e)
	}
	if e = s.Action(ctx, id, "approve", a.ID, "operator"); e != nil {
		t.Fatal(e)
	}
	got, _ = s.Task(ctx, id)
	if got.Status != "completed" {
		t.Fatal(got)
	}
}

func TestReviewBudgetAndCancellationRetainCapacityUntilAcknowledged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 60, 100}, "test"); e != nil {
		t.Fatal(e)
	}
	snap := Snapshot{Task: Definition{Reviewer: &Reviewer{}}}
	raw, _ := json.Marshal(snap)
	a.Snapshot = raw
	a.Workspace = t.TempDir()
	if e := s.Prepare(ctx, *a, a.Workspace, raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "test"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	w := &fakeWorkers{states: map[string]WorkerState{rounds[0].ID: {Exists: true, Alive: true, Busy: true, Launched: true, Tokens: 100}}, cancelError: ErrConflict}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	// Cancellation failure cannot free the task's slot even after the budget expires.
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	got, _ := s.Task(ctx, id)
	if got.Status != "running" {
		t.Fatal(got)
	}
	w.cancelError = nil
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	got, _ = s.Task(ctx, id)
	if got.Status != "blocked" || len(w.states) != 0 {
		t.Fatal(got, w.states)
	}
	raw, _ = json.Marshal(Submission{Outcome: "completed", Summary: "late"})
	if e := s.Submit(ctx, rounds[0].ID, capability(s.Config.InternalToken, rounds[0].ID), "result", raw); e == nil {
		t.Fatal("late submission accepted")
	}
}

func TestReviewWrongRoleAndDuplicateSubmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 60, 1000}, "test"); e != nil {
		t.Fatal(e)
	}
	snap := Snapshot{Task: Definition{Reviewer: &Reviewer{}}}
	raw, _ := json.Marshal(snap)
	a.Workspace = t.TempDir()
	if e := s.Prepare(ctx, *a, a.Workspace, raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "test"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	r := rounds[0]
	token := capability(s.Config.InternalToken, r.ID)
	raw, _ = json.Marshal(ReviewDecision{Decision: "accept", Summary: "bypass", Evidence: []string{"self"}})
	if e := s.Submit(ctx, r.ID, token, "review", raw); e == nil {
		t.Fatal("worker approved itself")
	}
	raw, _ = json.Marshal(Submission{Outcome: "blocked", Summary: "proposal"})
	if e := s.Submit(ctx, r.ID, "wrong", "result", raw); e == nil {
		t.Fatal("invalid capability accepted")
	}
	for i := 0; i < 2; i++ {
		if e := s.Submit(ctx, r.ID, token, "result", raw); e != nil {
			t.Fatal(e)
		}
	}
}

func TestInterruptedReviewerBlocksAndRetryStartsFreshWorker(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 60, 10000}, "test"); e != nil {
		t.Fatal(e)
	}
	snap := Snapshot{Task: Definition{Reviewer: &Reviewer{}}}
	raw, _ := json.Marshal(snap)
	a.Snapshot = raw
	a.Workspace = t.TempDir()
	if e := s.Prepare(ctx, *a, a.Workspace, raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "worker"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	payload := json.RawMessage(`{"outcome":"blocked","summary":"saved investigation"}`)
	if e := s.Submit(ctx, rounds[0].ID, capability(s.Config.InternalToken, rounds[0].ID), "result", payload); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "reviewer", "review"); e != nil {
		t.Fatal(e)
	}
	rounds, _ = s.Rounds(ctx, a.ID)
	w := &fakeWorkers{states: map[string]WorkerState{rounds[1].ID: {Exists: true, Alive: true, Launched: true, Busy: false}}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	got, _ := s.Task(ctx, id)
	if got.Status != "blocked" {
		t.Fatalf("reviewer failure retried task: %+v", got)
	}
	if e := s.Action(ctx, id, "retry", "", "operator"); e != nil {
		t.Fatal(e)
	}
	next := claim(t, s)
	next.Started = time.Now()
	next.Workspace = t.TempDir()
	next.Snapshot = raw
	if e := s.Prepare(ctx, *next, next.Workspace, raw); e != nil {
		t.Fatal(e)
	}
	if e := scheduler.tickReviewed(ctx, *next, snap, false, false); e != nil {
		t.Fatal(e)
	}
	recovered, _ := s.Rounds(ctx, next.ID)
	if len(recovered) != 1 || len(recovered[0].Submission) != 0 || w.launches != 0 {
		t.Fatalf("retry inherited an old proposal: %+v launches=%d", recovered, w.launches)
	}
	if e := scheduler.tickReviewed(ctx, *next, snap, false, false); e != nil {
		t.Fatal(e)
	}
	recovered, _ = s.Rounds(ctx, next.ID)
	if len(recovered) != 1 || recovered[0].Role != "worker" || w.launches != 1 {
		t.Fatalf("retry did not launch fresh worker: %+v", recovered)
	}
}

func TestStaleConversationCannotCancelNewAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	w := &fakeWorkers{states: map[string]WorkerState{a.ID: {Exists: true, Alive: true, Busy: true}}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.CancelTask(ctx, id, "operator", ID()); e == nil {
		t.Fatal("stale conversation cancelled current attempt")
	}
	if w.cancels != 0 {
		t.Fatal("cancel reached backend for stale attempt")
	}
	if e := scheduler.CancelTask(ctx, id, "operator", a.ID); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "cancelled" {
		t.Fatal(task)
	}
}

func TestReviewedAttemptUsesLiveControllerLimits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 60, 100}, "operator"); e != nil {
		t.Fatal(e)
	}
	a := claim(t, s)
	var saved map[string]any
	if e := json.Unmarshal(a.Snapshot, &saved); e != nil {
		t.Fatal(e)
	}
	if _, ok := saved["limits"]; ok {
		t.Fatal("new attempt snapshotted limits")
	}
	// Legacy snapshots must not override the live controller either.
	raw := json.RawMessage(`{"task":{"reviewer":{"max_rounds":3}},"limits":{"lease_secs":30,"task_limit_secs":60,"task_limit_tokens":100}}`)
	a.Workspace = t.TempDir()
	if e := s.Prepare(ctx, *a, a.Workspace, raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "test"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	w := &fakeWorkers{states: map[string]WorkerState{rounds[0].ID: {Exists: true, Alive: true, Busy: true, Launched: true, Tokens: 500}}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := s.ConfigureLimits(ctx, Limits{8, 90, 120, 1000}, "operator"); e != nil {
		t.Fatal(e)
	}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "running" || w.cancels != 0 {
		t.Fatal("live token increase ignored", task)
	}
	var ttl int
	if e := s.DB.QueryRowContext(ctx, "SELECT TIMESTAMPDIFF(SECOND,UTC_TIMESTAMP(6),lease_expires) FROM am_task_attempts WHERE id=?", a.ID).Scan(&ttl); e != nil {
		t.Fatal(e)
	}
	if ttl < 85 || ttl > 90 {
		t.Fatal("lease did not use updated controller setting", ttl)
	}
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 120, 400}, "operator"); e != nil {
		t.Fatal(e)
	}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ = s.Task(ctx, id)
	if task.Status != "blocked" {
		t.Fatal("lowered limit ignored", task)
	}
}

func TestLiveTimeLimitChangeAffectsRunningAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET started_at=TIMESTAMPADD(SECOND,-90,UTC_TIMESTAMP(6)) WHERE id=?", a.ID); e != nil {
		t.Fatal(e)
	}
	w := &fakeWorkers{states: map[string]WorkerState{a.ID: {Exists: true, Alive: true, Busy: true, Launched: true, Tokens: 1}}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 120, 1000}, "operator"); e != nil {
		t.Fatal(e)
	}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "running" {
		t.Fatal("increased time limit ignored", task)
	}
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 60, 1000}, "operator"); e != nil {
		t.Fatal(e)
	}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ = s.Task(ctx, id)
	if task.Status != "blocked" {
		t.Fatal("reduced time limit ignored", task)
	}
}

func TestReviewReserveCheckpointAndContinuation(t *testing.T) {
	for _, mode := range []string{"tokens", "time", "increased_limit"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id := enqueue(t, s, false, nil)
			resume(t, s, 1)
			a := claim(t, s)
			if e := s.ConfigureLimits(ctx, Limits{8, 30, 600, 1000}, "test"); e != nil {
				t.Fatal(e)
			}
			a.Started = time.Now()
			used := int64(900)
			if mode == "time" {
				a.Started = time.Now().Add(-541 * time.Second)
				used = 10
			}
			snap := Snapshot{Task: Definition{Reviewer: &Reviewer{}}}
			raw, _ := json.Marshal(snap)
			a.Snapshot = raw
			a.Workspace = t.TempDir()
			if e := s.Prepare(ctx, *a, a.Workspace, raw); e != nil {
				t.Fatal(e)
			}
			if e := s.newRound(ctx, *a, 1, "worker", "initial"); e != nil {
				t.Fatal(e)
			}
			rounds, _ := s.Rounds(ctx, a.ID)
			w := &fakeWorkers{states: map[string]WorkerState{rounds[0].ID: {Exists: true, Alive: true, Busy: true, Launched: true, Tokens: used}}}
			scheduler := Scheduler{s, w, "owner", func(string) {}}
			tick := func() {
				t.Helper()
				if e := scheduler.tickReviewed(ctx, *a, snap, false, false); e != nil {
					t.Fatal(e)
				}
			}
			tick()
			rounds, _ = s.Rounds(ctx, a.ID)
			var checkpoint Submission
			if e := json.Unmarshal(rounds[0].Submission, &checkpoint); e != nil {
				t.Fatal(e)
			}
			if checkpoint.Origin != "controller_checkpoint" || len(checkpoint.Blockers) != 1 {
				t.Fatal(checkpoint)
			}
			tick() // One reviewer may assess the interrupted checkpoint.
			rounds, _ = s.Rounds(ctx, a.ID)
			if len(rounds) != 2 || rounds[1].Role != "reviewer" {
				t.Fatal(rounds)
			}
			decision, _ := json.Marshal(ReviewDecision{Decision: "continue", Summary: "Useful work remains", Evidence: []string{"checkpoint"}, NextSteps: []string{"Continue experiment"}})
			if e := s.Submit(ctx, rounds[1].ID, capability(s.Config.InternalToken, rounds[1].ID), "review", decision); e != nil {
				t.Fatal(e)
			}
			if mode == "increased_limit" {
				if e := s.ConfigureLimits(ctx, Limits{8, 30, 1200, 2000}, "test"); e != nil {
					t.Fatal(e)
				}
			}
			tick()
			rounds, _ = s.Rounds(ctx, a.ID)
			got, _ := s.Task(ctx, id)
			if mode == "increased_limit" {
				if len(rounds) != 3 || rounds[2].Role != "worker" {
					t.Fatal(rounds)
				}
			} else if len(rounds) != 2 || got.Status != "blocked" || w.launches != 0 {
				t.Fatal("exhausted budget must not create another worker", got, rounds, w.launches)
			}
		})
	}
}
