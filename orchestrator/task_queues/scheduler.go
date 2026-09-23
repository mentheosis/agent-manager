package taskqueues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	orchestration "github.com/anthropics/agent-manager/orchestrator"
)

var ErrWorkerResumeRejected = errors.New("worker session continuation rejected")

type WorkerState struct {
	Tokens       int64   `json:"tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Exists       bool    `json:"exists"`
	Busy         bool    `json:"busy"`
	Alive        bool    `json:"alive"`
	Conversation string  `json:"conversation_id"`
	Launched     bool    `json:"launched"`
}
type Workers interface {
	Launch(context.Context, Attempt, Task, string, string) (WorkerState, error)
	State(context.Context, string) (WorkerState, error)
	Cancel(context.Context, string) error
}
type APIWorkers struct {
	Client *orchestration.Client
	Config Config
}

func (w *APIWorkers) path(id string) string {
	return "/api/task-queues/" + url.PathEscape(w.Config.Parent) + "/attempts/" + url.PathEscape(id)
}
func (w *APIWorkers) Launch(ctx context.Context, a Attempt, t Task, workspace, prompt string) (out WorkerState, err error) {
	var snap Snapshot
	if err = json.Unmarshal(a.Snapshot, &snap); err != nil {
		return
	}
	execution := snap.Task
	files := a.ReviewFiles
	if files == nil {
		files = map[string]string{}
	}
	err = w.Client.Request(ctx, http.MethodPost, w.path(a.ID), map[string]any{"task_id": t.ID, "workflow_id": t.Workflow, "attempt_number": t.AttemptCount, "workspace": workspace, "prompt": prompt, "execution": execution, "use_isolated_workspace": isolated(snap.UseIsolatedWorkspace), "repository": snap.Repository, "root_attempt": a.RootAttempt, "role": a.Role, "round": a.Round, "resume_worker": a.ResumeWorker, "resume_turn": a.ResumeTurn, "review_files": files}, &out, w.Config.InternalToken)
	var status *orchestration.HTTPStatusError
	if (a.ResumeWorker || a.ResumeTurn != "") && errors.As(err, &status) && status.StatusCode == http.StatusConflict {
		err = ErrWorkerResumeRejected
	}
	return
}
func (w *APIWorkers) State(ctx context.Context, id string) (out WorkerState, err error) {
	err = w.Client.Request(ctx, http.MethodGet, w.path(id), nil, &out, w.Config.InternalToken)
	return
}
func (w *APIWorkers) Cancel(ctx context.Context, id string) error {
	return w.Client.Request(ctx, http.MethodDelete, w.path(id), nil, nil, w.Config.InternalToken)
}

type Scheduler struct {
	Store   *Store
	Workers Workers
	Owner   string
	Log     func(string)
}

func (s *Scheduler) Run(ctx context.Context, locks ...*sync.Mutex) error {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if len(locks) > 0 {
			locks[0].Lock()
		}
		e := s.Tick(ctx)
		if len(locks) > 0 {
			locks[0].Unlock()
		}
		if e != nil && ctx.Err() == nil {
			message := e.Error()
			secrets := []string{s.Store.Config.DSN, s.Store.Config.InternalToken}
			if dsn, err := parseDatabaseDSN(s.Store.Config.DSN); err == nil {
				secrets = append(secrets, dsn.Passwd)
			}
			for _, secret := range secrets {
				if secret != "" {
					message = strings.ReplaceAll(message, secret, "[redacted]")
				}
			}
			s.Log("Queue cycle failed; scheduling will retry: " + message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (s *Scheduler) Tick(ctx context.Context) error {
	attempts, e := s.Store.Attempts(ctx)
	if e != nil {
		return e
	}
	for _, a := range attempts {
		expired := !a.Lease.After(time.Now().UTC())
		limits := s.Store.CurrentLimits()
		var input Snapshot
		if e := json.Unmarshal(a.Snapshot, &input); e != nil {
			return e
		}
		timedOut := time.Since(a.Started) > time.Duration(limits.TaskSeconds)*time.Second
		if a.Owner != s.Owner && !expired {
			continue
		}
		roundDriven := input.Task.Reviewer != nil
		if !roundDriven {
			rounds, err := s.Store.Rounds(ctx, a.ID)
			if err != nil {
				return err
			}
			roundDriven = len(rounds) > 0
		}
		if roundDriven && a.Workspace != "" {
			if e = s.tickReviewed(ctx, a, input, expired, timedOut); e != nil && !errors.Is(e, ErrConflict) {
				return e
			}
			continue
		}
		if expired || timedOut {
			// Do not release capacity while an old worker may still execute. An unreachable
			// backend keeps the slot reserved, even beyond lease expiry.
			if e = s.Workers.Cancel(ctx, a.ID); e != nil {
				continue
			}
			outcome, reason := "failed", "Worker lease expired"
			if timedOut && !expired {
				outcome, reason = "blocked", fmt.Sprintf("Time budget exhausted: controller limit %d seconds. Saved work is retained.", limits.TaskSeconds)
			}
			if e = s.Store.Finish(ctx, a, outcome, reason); e != nil {
				return e
			}
			continue
		}
		state, err := s.Workers.State(ctx, a.ID)
		if err != nil {
			continue
		}
		if state.Exists {
			usage, _ := json.Marshal(map[string]any{"tokens": state.Tokens, "cost_usd": state.CostUSD})
			if _, e = s.Store.DB.ExecContext(ctx, "UPDATE am_task_attempts SET resource_usage=JSON_MERGE_PATCH(COALESCE(resource_usage,JSON_OBJECT()),CAST(? AS JSON)) WHERE id=? AND owner=? AND status IN "+active, string(usage), a.ID, s.Owner); e != nil {
				return e
			}
			if limits.TaskTokens > 0 && state.Tokens >= limits.TaskTokens {
				if e = s.Workers.Cancel(ctx, a.ID); e != nil {
					continue
				}
				if e = s.Store.Finish(ctx, a, "blocked", fmt.Sprintf("Token budget exhausted: %d used / %d allowed. Saved work is retained.", state.Tokens, limits.TaskTokens)); e != nil {
					return e
				}
				continue
			}
		}
		if a.Status == "claimed" && !state.Exists {
			if e = s.launch(ctx, a); e != nil {
				return e
			}
			continue
		}
		if !state.Exists || !state.Alive {
			if state.Busy {
				continue
			}
			if e = s.Store.Finish(ctx, a, "ended", "Worker execution ended"); e != nil {
				return e
			}
			continue
		}
		if !state.Busy && state.Launched && time.Since(a.Started) > 5*time.Second {
			outcome := "ended"
			reason := "Worker execution ended; structured result required"
			if e = s.Store.Finish(ctx, a, outcome, reason); e != nil {
				return e
			}
			continue
		}
		if a.Status == "claimed" && state.Exists && state.Conversation != "" {
			if e = s.Store.Launched(ctx, a, state.Conversation); e != nil {
				return e
			}
		}
		if e = s.Store.Renew(ctx, a); e != nil && !errors.Is(e, ErrConflict) {
			return e
		}
	}
	if e = s.Store.ValidateDependencies(ctx); e != nil {
		return e
	}
	// Bound dispatch preparation to one new attempt per cycle so existing leases
	// are revisited between repository exports. This is a ramp-up rate, not a cap.
	a, _, e := s.Store.Claim(ctx, s.Owner)
	if e != nil {
		return e
	}
	if a == nil {
		return nil
	}
	return s.launch(ctx, *a)
}
func (s *Scheduler) launch(ctx context.Context, a Attempt) error {
	t, e := s.Store.Task(ctx, a.TaskID)
	if e != nil {
		return e
	}
	var upstream json.RawMessage
	if t.Depends.Valid {
		p, e := s.Store.Task(ctx, t.Depends.Int64)
		if e != nil {
			return e
		}
		upstream = p.Result
	}
	workspace := a.Workspace
	snapshot := a.Snapshot
	if workspace == "" {
		// Resolving/exporting also obeys the lease deadline. A slow export must not
		// launch after a different controller has fenced this attempt.
		resolveCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Store.CurrentLimits().LeaseSeconds/2)*time.Second)
		workspace, snapshot, e = resolveSnapshot(resolveCtx, s.Store.Config, t, a.ID, upstream, a.Snapshot)
		cancel()
		if e != nil {
			return s.Store.Finish(ctx, a, "blocked", "Definition or workspace could not be prepared: "+e.Error())
		}
		if e = s.Store.Prepare(ctx, a, workspace, snapshot); e != nil {
			return e
		}
	}
	var snap Snapshot
	if e = json.Unmarshal(snapshot, &snap); e != nil {
		return e
	}
	if e = s.Store.Renew(ctx, a); e != nil {
		return e
	}
	if snap.Task.Reviewer != nil {
		return s.Store.Renew(ctx, a)
	}
	a.Snapshot = snapshot
	state, e := s.Workers.Launch(ctx, a, t, workspace, snap.Prompt)
	if e != nil {
		return fmt.Errorf("worker launch pending reconciliation: %w", e)
	}
	return s.Store.Launched(ctx, a, state.Conversation)
}
func (s *Scheduler) CancelTask(ctx context.Context, id int64, actor string, expected ...string) error {
	t, e := s.Store.Task(ctx, id)
	if e != nil {
		return e
	}
	attemptID := ""
	if len(expected) > 0 {
		attemptID = expected[0]
	}
	if attemptID != "" && t.Latest != attemptID {
		return ErrConflict
	}
	if t.Status != "running" {
		return s.Store.Action(ctx, id, "cancel", attemptID, actor)
	}
	attempts, e := s.Store.Attempts(ctx)
	if e != nil {
		return e
	}
	for _, a := range attempts {
		if a.TaskID == id && a.ID == t.Latest {
			if e = s.cancelAttempt(ctx, a); e != nil {
				return e
			}
			a.Owner = actor
			return s.Store.Finish(ctx, a, "cancelled", "Cancelled by operator")
		}
	}
	return ErrConflict
}

// Stopping a conversation is a resumable interruption, not task cancellation.
// Use the existing blocked state to hold it without automatic retries or dispatch.
func (s *Scheduler) PauseConversation(ctx context.Context, id int64, attempt, turn, actor string) error {
	t, e := s.Store.Task(ctx, id)
	if e != nil {
		return e
	}
	if attempt == "" || t.Latest != attempt {
		return ErrConflict
	}
	rounds, e := s.Store.Rounds(ctx, attempt)
	if e != nil {
		return e
	}
	if (len(rounds) == 0 && turn != attempt) || (len(rounds) > 0 && rounds[len(rounds)-1].ID != turn) {
		return ErrConflict
	}
	if t.Status == "blocked" {
		return nil
	}
	if t.Status != "running" {
		return ErrConflict
	}
	attempts, e := s.Store.Attempts(ctx)
	if e != nil {
		return e
	}
	for _, a := range attempts {
		if a.ID != attempt || a.TaskID != id {
			continue
		}
		if a.Owner != s.Store.Config.ControllerID {
			return ErrConflict
		}
		// Stop execution and fence its capabilities before releasing the worker slot.
		if e = s.cancelAttempt(ctx, a); e != nil {
			return e
		}
		a.Owner = actor
		return s.Store.Finish(ctx, a, "blocked", "Paused by operator: conversation stopped. Send a message to the worker or reviewer to resume this attempt.")
	}
	return ErrConflict
}
