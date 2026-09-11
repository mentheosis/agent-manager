package taskqueues

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("AM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AM_TEST_MYSQL_DSN is required for MySQL integration tests")
	}
	c := Config{DSN: dsn, QueueID: "test", TablePrefix: "test_" + ID()[:10] + "_", MaxWorkersCeiling: 8, LeaseSeconds: 30, MaxAttemptSeconds: 60, WorkspaceRoot: t.TempDir(), InternalToken: strings.Repeat("s", 64)}
	s, e := Open(c)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	t.Cleanup(func() {
		for _, table := range []string{"am_work_log", "am_task_attempts", "am_tasks", "am_queues", "am_schema_version"} {
			s.DB.ExecContext(ctx, "DROP TABLE IF EXISTS "+table)
		}
		s.DB.Close()
	})
	if e = s.Init(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.Check(ctx); e != nil {
		t.Fatal(e)
	}
	return s
}
func enqueue(t *testing.T, s *Store, review bool, dep any) int64 {
	t.Helper()
	r, e := s.DB.ExecContext(context.Background(), "INSERT INTO am_tasks(queue_id,workflow_id,task_type,definition_ref,parameters,review_required,depends_on) VALUES (?,'workflow','neutral',JSON_OBJECT(),JSON_OBJECT(),?,?)", s.Config.QueueID, review, dep)
	if e != nil {
		t.Fatal(e)
	}
	id, _ := r.LastInsertId()
	return id
}
func resume(t *testing.T, s *Store, n int) {
	t.Helper()
	p := false
	if e := s.Configure(context.Background(), &n, &p, "test"); e != nil {
		t.Fatal(e)
	}
}
func claim(t *testing.T, s *Store) *Attempt {
	t.Helper()
	a, _, e := s.Claim(context.Background(), "owner")
	if e != nil || a == nil {
		t.Fatalf("claim: %v %v", a, e)
	}
	return a
}
func TestParallelLimitsAndLiveChanges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		enqueue(t, s, false, nil)
	}
	status, e := s.Status(ctx)
	if e != nil || status.Available != 12 || !status.Paused {
		t.Fatalf("initial status %+v %v", status, e)
	}
	for _, limit := range []int{1, 2} {
		resume(t, s, limit)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, _, e := s.Claim(ctx, s.Config.ControllerID); e != nil {
					t.Error(e)
				}
			}()
		}
		wg.Wait()
		status, e = s.Status(ctx)
		if e != nil || status.Workers != limit || status.Available != 12-limit {
			t.Fatalf("limit status %+v %v", status, e)
		}
	}
	max := 1
	if e = s.Configure(ctx, &max, nil, "operator"); e != nil {
		t.Fatal(e)
	}
	a, _, e := s.Claim(ctx, s.Config.ControllerID)
	if e != nil || a != nil {
		t.Fatalf("limit reduction launched worker: %v %v", a, e)
	}
	status, _ = s.Status(ctx)
	if status.Workers != 2 || status.MaxWorkers != 1 {
		t.Fatal(status)
	}
	max = 0
	if s.Configure(ctx, &max, nil, "operator") == nil {
		t.Fatal("accepted zero")
	}
	paused := true
	s.Configure(ctx, nil, &paused, "operator")
	status, _ = s.Status(ctx)
	if status.Available != 10 {
		t.Fatal("pause hid eligible backlog")
	}
	other, err := Open(s.Config)
	if err != nil {
		t.Fatal(err)
	}
	defer other.DB.Close()
	status, _ = other.Status(ctx)
	if status.MaxWorkers != 1 || !status.Paused {
		t.Fatal("settings not persisted")
	}
}
func TestSubmissionFencingAndReview(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := enqueue(t, s, true, nil)
	dependent := enqueue(t, s, false, id)
	resume(t, s, 2)
	a := claim(t, s)
	ws := t.TempDir()
	if e := s.Prepare(ctx, *a, ws, json.RawMessage(`{}`)); e != nil {
		t.Fatal(e)
	}
	payload := json.RawMessage(`{"outcome":"completed","summary":"verified"}`)
	token := capability(s.Config.InternalToken, a.ID)
	if s.Submit(ctx, a.ID, "wrong", "result", payload) == nil {
		t.Fatal("accepted invalid capability")
	}
	if e := s.Submit(ctx, a.ID, token, "result", payload); e != nil {
		t.Fatal(e)
	}
	if e := s.Submit(ctx, a.ID, token, "result", payload); e != nil {
		t.Fatal("duplicate rejected", e)
	}
	status, _ := s.Status(ctx)
	if status.Workers != 1 || status.Available != 0 {
		t.Fatal("submission released live worker or unblocked review dependency")
	}
	if e := s.Finish(ctx, *a, "submitted", "worker ended"); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, id)
	if task.Status != "awaiting_review" || len(task.Result) > 0 {
		t.Fatal(task)
	}
	if s.Action(ctx, id, "approve", "wrong", "human") == nil {
		t.Fatal("accepted stale review")
	}
	if e := s.Action(ctx, id, "approve", a.ID, "human"); e != nil {
		t.Fatal(e)
	}
	b := claim(t, s)
	if b.TaskID != dependent {
		t.Fatal("dependency not unlocked")
	}
	s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET lease_expires=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", b.ID)
	if s.Submit(ctx, b.ID, capability(s.Config.InternalToken, b.ID), "result", payload) == nil {
		t.Fatal("accepted expired worker")
	}
	if s.Renew(ctx, *b) == nil {
		t.Fatal("renewed expired lease")
	}
	logs, e := s.Logs(ctx, 0)
	if e != nil || len(logs) < 5 {
		t.Fatal(logs, e)
	}
}
func TestDependencyCyclesAndMissingResult(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := enqueue(t, s, false, nil)
	b := enqueue(t, s, false, a)
	s.DB.ExecContext(ctx, "UPDATE am_tasks SET depends_on=? WHERE id=?", b, a)
	if e := s.ValidateDependencies(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, a)
	if task.Status != "blocked" {
		t.Fatal(task)
	}
	enqueue(t, s, false, nil)
	resume(t, s, 1)
	attempt := claim(t, s)
	if e := s.Finish(ctx, *attempt, "failed", "no result"); e != nil {
		t.Fatal(e)
	}
	task, _ = s.Task(ctx, attempt.TaskID)
	if task.Status != "retry_wait" {
		t.Fatal(task)
	}
	status, _ := s.Status(ctx)
	if status.Workers != 0 || status.Available != 0 {
		t.Fatal(status)
	}
}

type fakeWorkers struct {
	states      map[string]WorkerState
	cancelError error
	launches    int
	cancels     int
}

func (w *fakeWorkers) Launch(_ context.Context, a Attempt, _ Task, _, _ string) (WorkerState, error) {
	w.launches++
	v := WorkerState{Exists: true, Alive: true, Busy: true, Launched: true, Conversation: "conversation"}
	w.states[a.ID] = v
	return v, nil
}
func (w *fakeWorkers) State(_ context.Context, id string) (WorkerState, error) {
	return w.states[id], nil
}
func (w *fakeWorkers) Cancel(_ context.Context, id string) error {
	w.cancels++
	if w.cancelError != nil {
		return w.cancelError
	}
	delete(w.states, id)
	return nil
}
func TestExpiredWorkersKeepCapacityUntilCancelled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET lease_expires=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", a.ID)
	w := &fakeWorkers{states: map[string]WorkerState{}, cancelError: ErrConflict}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	status, _ := s.Status(ctx)
	if status.Workers != 1 {
		t.Fatal("expired lease released capacity before stopping worker")
	}
	w.cancelError = nil
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	status, _ = s.Status(ctx)
	if status.Workers != 0 {
		t.Fatal(status)
	}
}
func TestReadyWorkerRequiresSubmissionAndRecoveryUsesExistingConversation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET started_at=TIMESTAMPADD(SECOND,-10,UTC_TIMESTAMP(6)) WHERE id=?", a.ID)
	w := &fakeWorkers{states: map[string]WorkerState{a.ID: {Exists: true, Alive: true, Launched: true, Busy: false}}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	task, _ := s.Task(ctx, a.TaskID)
	if task.Status == "completed" {
		t.Fatal("inferred success from ready")
	}
	if w.launches != 0 {
		t.Fatal("recovery launched duplicate")
	}
}

var _ = time.Second

func TestNeutralTaskReplayAndArtifactReview(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, task := definitionFixture(t)
	s.Config.Repositories = c.Repositories
	id := enqueue(t, s, true, nil)
	if _, e := s.DB.ExecContext(ctx, "UPDATE am_tasks SET definition_ref=?,parameters=? WHERE id=?", string(task.Definition), string(task.Parameters), id); e != nil {
		t.Fatal(e)
	}
	resume(t, s, 1)
	w := &fakeWorkers{states: map[string]WorkerState{}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	attempts, e := s.Attempts(ctx)
	if e != nil || len(attempts) != 1 {
		t.Fatal(attempts, e)
	}
	a := attempts[0]
	if a.Workspace == "" || len(a.Snapshot) == 0 || a.Conversation == "" {
		t.Fatal("missing durable launch snapshot", a)
	}
	// Same controller restarted between launch and outcome; recover the same worker.
	restarted := Scheduler{s, w, "owner", func(string) {}}
	if e = restarted.Tick(ctx); e != nil || w.launches != 1 {
		t.Fatal("duplicate launch", e)
	}
	if e = os.WriteFile(a.Workspace+"/report.md", []byte("Neutral report evidence"), 0600); e != nil {
		t.Fatal(e)
	}
	token := capability(s.Config.InternalToken, a.ID)
	if e = s.Submit(ctx, a.ID, token, "progress", json.RawMessage(`{"summary":"Inspected the pinned fixture"}`)); e != nil {
		t.Fatal(e)
	}
	if e = s.Submit(ctx, a.ID, token, "result", json.RawMessage(`{"outcome":"completed","summary":"Report ready for review","artifact_paths":["report.md"]}`)); e != nil {
		t.Fatal(e)
	}
	w.states[a.ID] = WorkerState{Exists: true, Alive: false, Launched: true}
	if e = restarted.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := s.Task(ctx, id)
	if e != nil || got.Status != "awaiting_review" || !strings.Contains(string(got.LatestResult), "sha256") {
		t.Fatal(got, e)
	}
	if e = s.Action(ctx, id, "reject", a.ID, "reviewer"); e != nil {
		t.Fatal(e)
	}
	if e = s.Action(ctx, id, "retry", "", "reviewer"); e != nil {
		t.Fatal(e)
	}
	if e = restarted.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	attempts, e = s.Attempts(ctx)
	if e != nil || len(attempts) != 1 || attempts[0].ID == a.ID || attempts[0].Workspace == a.Workspace {
		t.Fatal("retry reused prior attempt", attempts, e)
	}
	if e = s.Submit(ctx, a.ID, token, "progress", json.RawMessage(`{"summary":"stale write"}`)); e == nil {
		t.Fatal("stale attempt wrote after retry")
	}
}

func TestResourceBudgetStopsWorkerAndBlocksTask(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	enqueue(t, s, false, nil)
	resume(t, s, 1)
	a := claim(t, s)
	s.Config.MaxAttemptTokens = 100
	w := &fakeWorkers{states: map[string]WorkerState{a.ID: {Exists: true, Alive: true, Busy: true, Tokens: 101}}}
	scheduler := Scheduler{s, w, "owner", func(string) {}}
	if e := scheduler.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	got, e := s.Task(ctx, a.TaskID)
	if e != nil || got.Status != "blocked" || w.cancels != 1 {
		t.Fatal(got, e)
	}
}

func TestForeignBackendAttemptsAreNotRecovered(t *testing.T) {
	s := testStore(t)
	enqueue(t, s, false, nil)
	resume(t, s, 1)
	claim(t, s)
	cfg := s.Config
	cfg.InternalToken = strings.Repeat("different", 8)
	cfg.ControllerID = "another-backend"
	other, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer other.DB.Close()
	attempts, e := other.Attempts(context.Background())
	if e != nil || len(attempts) != 0 {
		t.Fatal("foreign attempts exposed for cancellation", attempts, e)
	}
}

func TestIndependentControllersSharePoolWithoutDuplicateClaims(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 12; i++ {
		enqueue(t, s, false, nil)
	}
	cfg := s.Config
	cfg.ControllerID = "second"
	other, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer other.DB.Close()
	resume(t, s, 2)
	resume(t, other, 2)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store := s
			if i%2 == 0 {
				store = other
			}
			if _, _, err := store.Claim(ctx, store.Config.ControllerID); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	a, _ := s.Status(ctx)
	b, _ := other.Status(ctx)
	if a.Workers != 2 || b.Workers != 2 || a.Available != 8 || b.Available != 8 {
		t.Fatal(a, b)
	}
	attempts, e := s.Attempts(ctx)
	if e != nil || len(attempts) != 4 {
		t.Fatal(attempts, e)
	}
	seen := map[int64]bool{}
	for _, attempt := range attempts {
		if seen[attempt.TaskID] {
			t.Fatal("double claim")
		}
		seen[attempt.TaskID] = true
	}
	paused := true
	one := 1
	if e = s.Configure(ctx, &one, &paused, "operator"); e != nil {
		t.Fatal(e)
	}
	b, _ = other.Status(ctx)
	if b.Paused || b.MaxWorkers != 2 {
		t.Fatal("controls leaked across controllers", b)
	}
	// A fresh controller gets independent defaults despite sharing the same pool.
	cfg.ControllerID = "third"
	fresh, e := Open(cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.DB.Close()
	c, _ := fresh.Status(ctx)
	if !c.Paused || c.MaxWorkers != 1 || c.Workers != 0 {
		t.Fatal(c)
	}
}
