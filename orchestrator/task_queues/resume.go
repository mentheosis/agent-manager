package taskqueues

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func budgetBlocked(reason string) bool {
	return strings.HasPrefix(reason, "Review round limit reached") || strings.HasPrefix(reason, "Time budget exhausted:") || strings.HasPrefix(reason, "Token budget exhausted:") || strings.HasPrefix(reason, "Controller time budget reserved for review:") || strings.HasPrefix(reason, "Controller token budget reserved for review:")
}

func (w *APIWorkers) PrepareResume(ctx context.Context, id string) error {
	return w.Client.Request(ctx, http.MethodPost, w.path(id)+"/resume", map[string]any{}, nil, w.Config.InternalToken)
}

func (s *Scheduler) ResumeTask(ctx context.Context, task int64, attempt, actor string) error {
	backend, ok := s.Workers.(interface {
		PrepareResume(context.Context, string) error
	})
	if !ok {
		return errors.New("worker backend does not support attempt resume")
	}
	return s.Store.resumeTask(ctx, task, attempt, actor, func() error { return backend.PrepareResume(ctx, attempt) })
}

// ConversationPrompt routes only the current role of a blocked attempt back into
// scheduling. Older/finished conversations are ordinary, detached discussions.
type ConversationPrompt struct {
	Turn string `json:"turn_id"`
	Text string `json:"text"`
}

var errDetachedConversation = errors.New("detached conversation")

func (w *APIWorkers) PrepareConversationResume(ctx context.Context, id, turn string) error {
	return w.Client.Request(ctx, http.MethodPost, w.path(id)+"/resume", map[string]any{"turn_id": turn}, nil, w.Config.InternalToken)
}

func (s *Scheduler) PromptTask(ctx context.Context, task int64, attempt, actor string, prompt ConversationPrompt) (bool, error) {
	if strings.TrimSpace(prompt.Text) == "" || len(prompt.Text) > 1000000 {
		return false, errors.New("Prompt must contain 1..1000000 bytes")
	}
	backend, ok := s.Workers.(interface {
		PrepareConversationResume(context.Context, string, string) error
	})
	if !ok {
		return false, errors.New("worker backend does not support attempt resume")
	}
	e := s.Store.resumeTask(ctx, task, attempt, actor, func() error { return backend.PrepareConversationResume(ctx, attempt, prompt.Turn) }, prompt)
	if errors.Is(e, errDetachedConversation) {
		return false, nil
	}
	return e == nil, e
}

// The task lock fences concurrent retry/cancel/resume. The capacity mutex is shared
// with Claim. Backend preparation only removes the root cancellation fence; all
// old round capabilities remain cancelled. No prompt is sent until SQL commits.
func (s *Store) resumeTask(ctx context.Context, task int64, attempt, actor string, prepare func() error, messages ...ConversationPrompt) error {
	s.controls.mu.Lock()
	defer s.controls.mu.Unlock()
	return s.transaction(ctx, func(tx *Tx) error {
		var status, latest string
		if e := tx.QueryRowContext(ctx, "SELECT status,COALESCE(latest_attempt,'') FROM am_tasks WHERE id=? AND queue_id=? FOR UPDATE", task, s.Config.QueueID).Scan(&status, &latest); e != nil {
			if len(messages) > 0 && errors.Is(e, sql.ErrNoRows) {
				return errDetachedConversation
			}
			return e
		}
		var message *ConversationPrompt
		role := "worker"
		if len(messages) > 0 {
			message = &messages[0]
			if latest != attempt || status == "completed" || status == "queued" || status == "retry_wait" {
				return errDetachedConversation
			}
			var roundCount int
			if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM am_task_rounds WHERE attempt_id=?", attempt).Scan(&roundCount); e != nil {
				return e
			}
			if roundCount == 0 && message.Turn == attempt {
				role = "worker" // Original single-worker executions have no round row.
			} else {
				var current string
				if e := tx.QueryRowContext(ctx, "SELECT role FROM am_task_rounds WHERE id=? AND attempt_id=?", message.Turn, attempt).Scan(&role); e != nil {
					if errors.Is(e, sql.ErrNoRows) {
						return errDetachedConversation
					}
					return e
				}
				if e := tx.QueryRowContext(ctx, "SELECT id FROM am_task_rounds WHERE attempt_id=? AND role=? ORDER BY round_number DESC LIMIT 1", attempt, role).Scan(&current); e != nil {
					return e
				}
				if current != message.Turn {
					var pending bool
					if e := tx.QueryRowContext(ctx, "SELECT COALESCE(JSON_UNQUOTE(JSON_EXTRACT(resource_usage,'$.resume_turn')),'')=? AND COALESCE(conversation_id,'')='' FROM am_task_rounds WHERE id=?", message.Turn, current).Scan(&pending); e != nil {
						return e
					}
					if pending {
						return errors.New("A continuation for this conversation is already queued; wait for it to start")
					}
					return errDetachedConversation
				}
				// An older reviewer must not reopen review after a newer worker started.
				var newest string
				if e := tx.QueryRowContext(ctx, "SELECT id FROM am_task_rounds WHERE attempt_id=? ORDER BY round_number DESC,FIELD(role,'worker','reviewer') DESC LIMIT 1", attempt).Scan(&newest); e != nil {
					return e
				}
				if role == "reviewer" && newest != message.Turn {
					return errDetachedConversation
				}
			}
			if status != "blocked" && status != "failed" && status != "cancelled" && status != "awaiting_review" {
				return errors.New("This task is still active; wait for it to stop before sending a continuation")
			}
		} else if attempt == "" || latest != attempt || status != "blocked" {
			return ErrConflict
		}
		var reason, owner, backend, attemptStatus string
		var snapshot, usage json.RawMessage
		var started, stopped time.Time
		if e := tx.QueryRowContext(ctx, "SELECT status,owner,backend_id,COALESCE(error_text,''),input_snapshot,COALESCE(resource_usage,JSON_OBJECT()),started_at,completed_at FROM am_task_attempts WHERE id=? FOR UPDATE", attempt).Scan(&attemptStatus, &owner, &backend, &reason, &snapshot, &usage, &started, &stopped); e != nil {
			return e
		}
		if message == nil && (attemptStatus != "blocked" || !budgetBlocked(reason)) {
			return errors.New("Resume is available only for controller time/token/review-round limit interruptions; use Retry for other failures")
		}
		if owner != s.Config.ControllerID || backend != s.backendID() {
			return errors.New("Resume requires the original controller and backend")
		}
		var snap Snapshot
		if e := json.Unmarshal(snapshot, &snap); e != nil {
			return e
		}
		if s.controls.Paused {
			return errors.New("Start/resume the controller before resuming a task")
		}
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM am_task_attempts WHERE queue_id=? AND owner=? AND backend_id=? AND status IN "+active, s.Config.QueueID, owner, backend).Scan(&n); e != nil {
			return e
		}
		if n >= s.controls.MaxWorkers {
			return errors.New("No worker slot available; wait or increase max workers")
		}
		var consumed struct {
			Tokens       int64 `json:"tokens"`
			PausedMicros int64 `json:"paused_micros"`
		}
		if e := json.Unmarshal(usage, &consumed); e != nil {
			return e
		}
		limits := s.controls.Limits
		elapsed := stopped.Sub(started) - time.Duration(consumed.PausedMicros)*time.Microsecond
		tokenCutoff := limits.TaskTokens * 9 / 10
		timeCutoff := time.Duration(limits.TaskSeconds) * time.Second * 9 / 10
		if role == "reviewer" || snap.Task.Reviewer == nil {
			tokenCutoff = limits.TaskTokens
			timeCutoff = time.Duration(limits.TaskSeconds) * time.Second
		}
		if consumed.Tokens >= tokenCutoff {
			return fmt.Errorf("Increase the token limit before Resume: %d tokens consumed; current worker cutoff %d", consumed.Tokens, tokenCutoff)
		}
		if elapsed >= timeCutoff {
			return fmt.Errorf("Increase the time limit before Resume: %d seconds consumed; current worker cutoff %d", int64(elapsed.Seconds()), limits.TaskSeconds*9/10)
		}
		rounds, e := s.Rounds(ctx, attempt)
		if e != nil {
			return e
		}
		legacyWorker := len(rounds) == 0 && snap.Task.Reviewer == nil
		if legacyWorker {
			var conversation string
			if e := tx.QueryRowContext(ctx, "SELECT COALESCE(conversation_id,'') FROM am_task_attempts WHERE id=?", attempt).Scan(&conversation); e != nil {
				return e
			}
			rounds = []Round{{ID: attempt, Number: 1, Role: "worker", Conversation: conversation}}
		}
		if len(rounds) == 0 {
			return errors.New("No saved worker rounds; use Retry")
		}
		if rounds[len(rounds)-1].Number >= limits.ReviewRounds {
			return fmt.Errorf("Increase the review-round limit before Resume: %d rounds used / %d allowed", rounds[len(rounds)-1].Number, limits.ReviewRounds)
		}
		prompt := "Controller resumed this attempt after a budget interruption. Continue your unfinished work from the retained conversation and files, then submit your updated result."
		for i := len(rounds) - 1; i >= 0; i-- {
			if rounds[i].Role == "reviewer" && len(rounds[i].Submission) > 0 {
				var d ReviewDecision
				if json.Unmarshal(rounds[i].Submission, &d) == nil && d.Decision == "continue" {
					feedback, _ := json.MarshalIndent(d, "", "  ")
					prompt = "Reviewer feedback:\n" + string(feedback)
					break
				}
			}
		}
		if message != nil {
			prompt = message.Text
		}
		if role == "reviewer" {
			var proposal Round
			for _, r := range rounds {
				if r.Role == "worker" {
					proposal = r
				}
			}
			if len(proposal.Submission) == 0 {
				return errors.New("No worker proposal is available for review")
			}
		}
		if e = prepare(); e != nil {
			return fmt.Errorf("Original worker could not be prepared for Resume; ensure its conversation still exists and all attempt conversations have stopped: %w", e)
		}
		if legacyWorker {
			if _, e = tx.ExecContext(ctx, "INSERT INTO am_task_rounds(id,attempt_id,round_number,role,prompt,submission,conversation_id,resource_usage,completed_at) SELECT id,id,1,'worker',?,submission,conversation_id,resource_usage,completed_at FROM am_task_attempts WHERE id=?", snap.Prompt, attempt); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE am_task_rounds SET completed_at=COALESCE(completed_at,?) WHERE attempt_id=? AND submission IS NULL", stopped, attempt); e != nil {
			return e
		}
		number := rounds[len(rounds)-1].Number + 1
		hash := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", attempt, number, role)))
		roundID := hex.EncodeToString(hash[:16])
		if _, e = tx.ExecContext(ctx, "INSERT INTO am_task_rounds(id,attempt_id,round_number,role,prompt,resource_usage) VALUES(?,?,?,?,?,JSON_OBJECT('resume_turn',?))", roundID, attempt, number, role, prompt, func() string {
			if message != nil {
				return message.Turn
			}
			return ""
		}()); e != nil {
			return e
		}
		// Keep original start time and cumulative usage. Exclude only blocked downtime.
		if _, e = tx.ExecContext(ctx, "UPDATE am_task_attempts SET status='running',completed_at=NULL,error_text=NULL,submission=NULL,lease_expires=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)),heartbeat_at=UTC_TIMESTAMP(6),resource_usage=JSON_SET(COALESCE(resource_usage,JSON_OBJECT()),'$.paused_micros',? + GREATEST(0,TIMESTAMPDIFF(MICROSECOND,?,UTC_TIMESTAMP(6)))) WHERE id=?", limits.LeaseSeconds, consumed.PausedMicros, stopped, attempt); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE am_tasks SET status='running',updated_at=UTC_TIMESTAMP(6) WHERE id=?", task); e != nil {
			return e
		}
		if message != nil {
			return s.log(ctx, tx, task, attempt, actor, "human_feedback", "Resumed "+role+" from a conversation prompt", map[string]any{"round": number, "turn_id": roundID, "previous_turn": message.Turn, "prompt": message.Text})
		}
		return s.log(ctx, tx, task, attempt, actor, "resumed", "Resumed the same attempt with retained worker context and cumulative usage", map[string]any{"round": number, "turn_id": roundID})
	})
}
