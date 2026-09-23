package taskqueues

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestResumeBudgetAttemptRetainsIdentityUsageAndContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	snap := Snapshot{Task: Definition{Reviewer: &Reviewer{}}}
	raw, _ := json.Marshal(snap)
	if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "original assignment"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET conversation_id='retained-worker',resource_usage=JSON_OBJECT('tokens',900),submission=JSON_OBJECT('outcome','blocked','summary','checkpoint') WHERE id=?", rounds[0].ID); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "reviewer", "review"); e != nil {
		t.Fatal(e)
	}
	feedback := ReviewDecision{Decision: "continue", Summary: "Continue useful research", Evidence: []string{"checkpoint"}, NextSteps: []string{"Run the missing experiment"}}
	decision, _ := json.Marshal(feedback)
	rounds, _ = s.Rounds(ctx, a.ID)
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET submission=? WHERE id=?", string(decision), rounds[1].ID); e != nil {
		t.Fatal(e)
	}
	if e := s.Finish(ctx, *a, "blocked", "Time budget exhausted: controller limit 600 seconds."); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET started_at=TIMESTAMPADD(SECOND,-4200,UTC_TIMESTAMP(6)),completed_at=TIMESTAMPADD(SECOND,-3600,UTC_TIMESTAMP(6)),resource_usage=JSON_OBJECT('tokens',900) WHERE id=?", a.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 600, 1000}, "test"); e != nil {
		t.Fatal(e)
	}
	prepared := 0
	prepare := func() error { prepared++; return nil }
	if e := s.resumeTask(ctx, id, a.ID, "test", prepare); e == nil {
		t.Fatal("unchanged limits accepted")
	}
	if prepared != 0 {
		t.Fatal("backend called before validation")
	}
	if e := s.ConfigureLimits(ctx, Limits{8, 30, 1200, 2000}, "test"); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "test", func() error { return errors.New("missing worker") }); e == nil {
		t.Fatal("missing worker accepted")
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "blocked" {
		t.Fatal(task)
	}
	if e := s.resumeTask(ctx, id, a.ID, "test", prepare); e != nil {
		t.Fatal(e)
	}
	task, _ = s.Task(ctx, id)
	if task.Status != "running" || task.AttemptCount != 1 || task.Latest != a.ID {
		t.Fatal(task)
	}
	attempts, e := s.Attempts(ctx)
	if e != nil || len(attempts) != 1 {
		t.Fatal(attempts, e)
	}
	elapsed := time.Since(attempts[0].Started)
	if elapsed < 599*time.Second || elapsed > 605*time.Second {
		t.Fatal("blocked downtime counted", elapsed)
	}
	rounds, _ = s.Rounds(ctx, a.ID)
	expected, _ := json.MarshalIndent(feedback, "", "  ")
	if len(rounds) != 3 || rounds[2].Prompt != "Reviewer feedback:\n"+string(expected) {
		t.Fatal(rounds)
	}
	if e := s.resumeTask(ctx, id, a.ID, "test", prepare); e == nil || prepared != 1 {
		t.Fatal("duplicate resume dispatched", e, prepared)
	}
	// The next scheduler tick requests the existing worker, and retains paused time.
	w := &fakeWorkers{states: map[string]WorkerState{}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	var persisted string
	if e := s.DB.QueryRowContext(ctx, "SELECT resource_usage FROM am_task_attempts WHERE id=?", a.ID).Scan(&persisted); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(persisted, "paused_micros") || !strings.Contains(persisted, "900") {
		t.Fatal(persisted)
	}
}

func TestReviewLimitIsLiveAndRoundBlockedAttemptResumes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	// Existing snapshots can still contain the retired per-task setting.
	raw := json.RawMessage(`{"task":{"reviewer":{"max_rounds":1}}}`)
	var snap Snapshot
	if e := json.Unmarshal(raw, &snap); e != nil {
		t.Fatal(e)
	}
	if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
		t.Fatal(e)
	}
	a.Started = time.Now()
	a.Workspace = t.TempDir()
	if e := s.newRound(ctx, *a, 1, "worker", "initial"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET conversation_id='worker',submission=JSON_OBJECT('outcome','blocked','summary','more work') WHERE id=?", rounds[0].ID)
	if e := s.newRound(ctx, *a, 1, "reviewer", "review"); e != nil {
		t.Fatal(e)
	}
	rounds, _ = s.Rounds(ctx, a.ID)
	decision := `{"decision":"continue","summary":"Continue","evidence":["evidence"],"next_steps":["next"]}`
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET submission=? WHERE id=?", decision, rounds[1].ID); e != nil {
		t.Fatal(e)
	}
	if e := s.ConfigureLimits(ctx, Limits{1, 30, 600, 10000}, "test"); e != nil {
		t.Fatal(e)
	}
	w := &fakeWorkers{states: map[string]WorkerState{}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.tickReviewed(ctx, *a, snap, false, false); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "blocked" || !strings.Contains(task.LatestError, "1 used / 1 allowed") {
		t.Fatal(task)
	}
	if e := s.resumeTask(ctx, id, a.ID, "test", func() error { return nil }); e == nil {
		t.Fatal("unchanged round limit accepted")
	}
	if e := s.ConfigureLimits(ctx, Limits{3, 30, 600, 10000}, "test"); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "test", func() error { return nil }); e != nil {
		t.Fatal(e)
	}
	rounds, _ = s.Rounds(ctx, a.ID)
	if len(rounds) != 3 || rounds[2].Number != 2 {
		t.Fatal(rounds)
	}
	task, _ = s.Task(ctx, id)
	if task.AttemptCount != 1 || task.Latest != a.ID {
		t.Fatal(task)
	}
	// The retired snapshot limit of 1 must not block the resumed attempt's
	// second review; the current controller permits three rounds.
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET submission=JSON_OBJECT('outcome','blocked','summary','new evidence') WHERE id=?", rounds[2].ID); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 2, "reviewer", "review again"); e != nil {
		t.Fatal(e)
	}
	rounds, _ = s.Rounds(ctx, a.ID)
	next := `{"decision":"continue","summary":"More work","evidence":["new evidence"],"next_steps":["another experiment"]}`
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET submission=? WHERE id=?", next, rounds[3].ID); e != nil {
		t.Fatal(e)
	}
	if e := scheduler.tickReviewed(ctx, *a, snap, false, false); e != nil {
		t.Fatal(e)
	}
	rounds, _ = s.Rounds(ctx, a.ID)
	if len(rounds) != 5 || rounds[4].Number != 3 {
		t.Fatal(rounds)
	}
}

func TestHumanPromptResumesWorkerOrReviewerWithoutReplacingAttempt(t *testing.T) {
	for _, role := range []string{"worker", "reviewer"} {
		t.Run(role, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			id := enqueue(t, s, false, nil)
			resume(t, s, 1)
			a := claim(t, s)
			raw, _ := json.Marshal(Snapshot{Task: Definition{Reviewer: &Reviewer{}}})
			if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
				t.Fatal(e)
			}
			if e := s.newRound(ctx, *a, 1, "worker", "initial"); e != nil {
				t.Fatal(e)
			}
			rounds, _ := s.Rounds(ctx, a.ID)
			workerID := rounds[0].ID
			if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET conversation_id='worker',submission=JSON_OBJECT('outcome','blocked','summary','Need data') WHERE id=?", workerID); e != nil {
				t.Fatal(e)
			}
			if e := s.newRound(ctx, *a, 1, "reviewer", "review"); e != nil {
				t.Fatal(e)
			}
			rounds, _ = s.Rounds(ctx, a.ID)
			reviewerID := rounds[1].ID
			if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET conversation_id='reviewer',submission=JSON_OBJECT('decision','escalate','summary','Need source','external_dependency','Node export') WHERE id=?", reviewerID); e != nil {
				t.Fatal(e)
			}
			if e := s.Finish(ctx, *a, "blocked", "Node export needed"); e != nil {
				t.Fatal(e)
			}
			turn := workerID
			if role == "reviewer" {
				turn = reviewerID
			}
			prompt := ConversationPrompt{Turn: turn, Text: "Please find a source for the extract"}
			called := 0
			prepare := func() error { called++; return nil }
			if e := s.resumeTask(ctx, id, a.ID, "operator", prepare, prompt); e != nil {
				t.Fatal(e)
			}
			task, _ := s.Task(ctx, id)
			if task.Status != "running" || task.AttemptCount != 1 || task.Latest != a.ID || called != 1 {
				t.Fatal(task, called)
			}
			rounds, _ = s.Rounds(ctx, a.ID)
			r := rounds[len(rounds)-1]
			if r.Role != role || r.Number != 2 || r.Prompt != prompt.Text || r.ResumeTurn != turn {
				t.Fatal(r)
			}
			// Old review cannot affect the new round.
			if e := s.resumeTask(ctx, id, a.ID, "operator", prepare, ConversationPrompt{reviewerID, "Old discussion"}); role == "worker" && !errors.Is(e, errDetachedConversation) || role == "reviewer" && (e == nil || !strings.Contains(e.Error(), "already queued")) {
				t.Fatal(e)
			}
			if role == "reviewer" {
				decision := ReviewDecision{Decision: "continue", Summary: "Acquire the public source", Evidence: []string{"saved proposal"}, NextSteps: []string{"Find a node snapshot"}}
				payload, _ := json.Marshal(decision)
				handled, e := s.submitRound(ctx, r.ID, capability(s.Config.InternalToken, r.ID), "review", payload)
				if e != nil || !handled {
					t.Fatal(handled, e)
				}
				// Resumed review still validates against the original worker proposal.
				w := &fakeWorkers{states: map[string]WorkerState{}}
				scheduler := Scheduler{s, w, "owner", func(string) {}}
				if e := scheduler.Tick(ctx); e != nil {
					t.Fatal(e)
				}
				rounds, _ = s.Rounds(ctx, a.ID)
				if rounds[len(rounds)-1].Role != "worker" || !strings.Contains(rounds[len(rounds)-1].Prompt, "Find a node snapshot") {
					t.Fatal(rounds)
				}
			}
		})
	}
}

func TestHumanPromptRespectsPauseLimitsAndHistoricalAttempts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	raw, _ := json.Marshal(Snapshot{Task: Definition{Reviewer: &Reviewer{}}})
	if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "initial"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	if e := s.Finish(ctx, *a, "blocked", "Need source"); e != nil {
		t.Fatal(e)
	}
	prompt := ConversationPrompt{rounds[0].ID, "Continue"}
	called := 0
	prepare := func() error { called++; return nil }
	paused := true
	if e := s.Configure(ctx, nil, &paused, "test"); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "operator", prepare, prompt); e == nil || !strings.Contains(e.Error(), "controller") {
		t.Fatal(e)
	}
	resume(t, s, 1)
	if e := s.ConfigureLimits(ctx, Limits{1, 30, 60, 2000000}, "test"); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "operator", prepare, prompt); e == nil || !strings.Contains(e.Error(), "review-round") {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_tasks SET latest_attempt=? WHERE id=?", ID(), id); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "operator", prepare, prompt); !errors.Is(e, errDetachedConversation) {
		t.Fatal(e)
	}
	if called != 0 {
		t.Fatal("backend ran before eligibility checks")
	}
}

func TestConversationStopPausesAndPromptResumesSameAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	raw, _ := json.Marshal(Snapshot{Task: Definition{Reviewer: &Reviewer{}}})
	if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "initial"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	turn := rounds[0].ID
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_rounds SET conversation_id='saved-worker' WHERE id=?", turn); e != nil {
		t.Fatal(e)
	}
	w := &fakeWorkers{states: map[string]WorkerState{}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.PauseConversation(ctx, id, a.ID, "wrong-round", "operator"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	w.cancelError = errors.New("cannot stop")
	if e := scheduler.PauseConversation(ctx, id, a.ID, turn, "operator"); e == nil {
		t.Fatal("pause accepted before worker stopped")
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "running" {
		t.Fatal(task)
	}
	w.cancelError = nil
	if e := scheduler.PauseConversation(ctx, id, a.ID, turn, "operator"); e != nil {
		t.Fatal(e)
	}
	task, _ = s.Task(ctx, id)
	if task.Status != "blocked" || task.Latest != a.ID || task.AttemptCount != 1 {
		t.Fatal(task)
	}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if w.launches != 0 {
		t.Fatal("paused attempt restarted automatically")
	}
	if e := s.resumeTask(ctx, id, a.ID, "operator", func() error { return nil }, ConversationPrompt{turn, "Here is the answer to your question"}); e != nil {
		t.Fatal(e)
	}
	task, _ = s.Task(ctx, id)
	rounds, _ = s.Rounds(ctx, a.ID)
	if task.Status != "running" || task.Latest != a.ID || task.AttemptCount != 1 || rounds[len(rounds)-1].Prompt != "Here is the answer to your question" {
		t.Fatal(task, rounds)
	}
}

func TestPromptCanReopenLatestPreviouslyCancelledAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	raw, _ := json.Marshal(Snapshot{Task: Definition{Reviewer: &Reviewer{}}})
	if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
		t.Fatal(e)
	}
	if e := s.newRound(ctx, *a, 1, "worker", "initial"); e != nil {
		t.Fatal(e)
	}
	rounds, _ := s.Rounds(ctx, a.ID)
	if e := s.Finish(ctx, *a, "cancelled", "Cancelled by operator"); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "operator", func() error { return nil }, ConversationPrompt{rounds[0].ID, "Continue with my answer"}); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "running" || task.Latest != a.ID || task.AttemptCount != 1 {
		t.Fatal(task)
	}
}

func TestSingleWorkerPromptResumesWithoutReviewer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	raw, _ := json.Marshal(Snapshot{Prompt: "Original task", Task: Definition{}})
	if e := s.Prepare(ctx, *a, t.TempDir(), raw); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET conversation_id='original-worker',submission=JSON_OBJECT('outcome','blocked','summary','Need clarification'),resource_usage=JSON_OBJECT('tokens',123) WHERE id=?", a.ID); e != nil {
		t.Fatal(e)
	}
	if e := s.Finish(ctx, *a, "blocked", "Need clarification"); e != nil {
		t.Fatal(e)
	}
	if e := s.resumeTask(ctx, id, a.ID, "operator", func() error { return nil }, ConversationPrompt{a.ID, "Here is the answer"}); e != nil {
		t.Fatal(e)
	}
	rounds, e := s.Rounds(ctx, a.ID)
	if e != nil || len(rounds) != 2 {
		t.Fatal(rounds, e)
	}
	if rounds[0].Tokens != 123 || rounds[1].ResumeTurn != a.ID || rounds[1].Prompt != "Here is the answer" {
		t.Fatal(rounds)
	}
	w := &fakeWorkers{states: map[string]WorkerState{}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if w.launches != 1 {
		t.Fatal(w.launches)
	}
	payload := json.RawMessage(`{"outcome":"completed","summary":"Implemented"}`)
	if _, e := s.submitRound(ctx, rounds[1].ID, capability(s.Config.InternalToken, rounds[1].ID), "result", payload); e != nil {
		t.Fatal(e)
	}
	w.states[rounds[1].ID] = WorkerState{Exists: true, Launched: true}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "completed" || task.AttemptCount != 1 {
		t.Fatal(task)
	}
}
