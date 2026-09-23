package taskqueues

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type Round struct {
	ResumeTurn   string          `json:"-"`
	SubmittedAt  *time.Time      `json:"submitted_at,omitempty"`
	CostUSD      float64         `json:"cost_usd"`
	ID           string          `json:"id"`
	Number       int             `json:"number"`
	Role         string          `json:"role"`
	Prompt       string          `json:"-"`
	Submission   json.RawMessage `json:"submission,omitempty"`
	Conversation string          `json:"conversation_id,omitempty"`
	Tokens       int64           `json:"tokens"`
}
type ReviewDecision struct {
	Decision           string   `json:"decision"`
	Summary            string   `json:"summary"`
	Evidence           []string `json:"evidence"`
	NextSteps          []string `json:"next_steps,omitempty"`
	ExternalDependency string   `json:"external_dependency,omitempty"`
}

func (d ReviewDecision) validate(proposal Submission) error {
	if strings.TrimSpace(d.Summary) == "" || len(d.Summary) > 16000 {
		return errors.New("review requires a summary (max 16000 characters)")
	}
	nonempty := func(xs []string) bool {
		if len(xs) == 0 {
			return false
		}
		for _, x := range xs {
			if strings.TrimSpace(x) == "" {
				return false
			}
		}
		return true
	}
	if !nonempty(d.Evidence) {
		return errors.New("review must cite supporting evidence")
	}
	switch d.Decision {
	case "continue":
		if !nonempty(d.NextSteps) {
			return errors.New("continue requires concrete next_steps")
		}
	case "accept":
		if proposal.Outcome != "completed" {
			return errors.New("only a completed proposal can be accepted")
		}
	case "escalate":
		if strings.TrimSpace(d.ExternalDependency) == "" {
			return errors.New("escalate requires an external_dependency and human action")
		}
	default:
		return errors.New("decision must be continue, accept, or escalate")
	}
	return nil
}
func (s *Store) Rounds(ctx context.Context, attempt string) ([]Round, error) {
	rows, e := s.DB.QueryContext(ctx, `SELECT id,round_number,role,prompt,COALESCE(submission,'null'),COALESCE(conversation_id,''),COALESCE(JSON_EXTRACT(resource_usage,'$.tokens'),0),COALESCE(JSON_EXTRACT(resource_usage,'$.cost_usd'),0),completed_at,COALESCE(JSON_UNQUOTE(JSON_EXTRACT(resource_usage,'$.resume_turn')),'') FROM am_task_rounds WHERE attempt_id=? ORDER BY round_number, FIELD(role,'worker','reviewer')`, attempt)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Round{}
	for rows.Next() {
		var r Round
		if e = rows.Scan(&r.ID, &r.Number, &r.Role, &r.Prompt, &r.Submission, &r.Conversation, &r.Tokens, &r.CostUSD, &r.SubmittedAt, &r.ResumeTurn); e != nil {
			return nil, e
		}
		if string(r.Submission) == "null" {
			r.Submission = nil
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) newRound(ctx context.Context, a Attempt, n int, role, prompt string) error {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%s", a.ID, n, role)))
	id := hex.EncodeToString(h[:16])
	return s.transaction(ctx, func(tx *Tx) error {
		var valid bool
		if e := tx.QueryRowContext(ctx, "SELECT owner=? AND lease_expires>UTC_TIMESTAMP(6) AND status IN "+active+" FROM am_task_attempts WHERE id=? FOR UPDATE", a.Owner, a.ID).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrConflict
		}
		result, e := tx.ExecContext(ctx, "INSERT IGNORE INTO am_task_rounds(id,attempt_id,round_number,role,prompt) VALUES(?,?,?,?,?)", id, a.ID, n, role, prompt)
		if e != nil {
			return e
		}
		count, _ := result.RowsAffected()
		if count == 0 {
			return nil
		}
		return s.log(ctx, tx, a.TaskID, a.ID, "controller", role+"_round", fmt.Sprintf("%s · round %d", role, n), map[string]any{"round": n, "role": role, "turn_id": id})
	})
}

// Round capabilities cannot submit for another role, turn, or expired attempt.
func (s *Store) submitRound(ctx context.Context, id, token, kind string, payload json.RawMessage) (bool, error) {
	var attempt string
	e := s.DB.QueryRowContext(ctx, "SELECT r.attempt_id FROM am_task_rounds r JOIN am_task_attempts a ON a.id=r.attempt_id WHERE r.id=? AND a.queue_id=?", id, s.Config.QueueID).Scan(&attempt)
	if e != nil {
		if errors.Is(e, sql.ErrNoRows) {
			return false, nil
		}
		return true, e
	}
	if !hmac.Equal([]byte(capability(s.Config.InternalToken, id)), []byte(token)) {
		return true, errors.New("invalid round capability")
	}
	return true, s.transaction(ctx, func(tx *Tx) error {
		var status, workspace string
		var alive bool
		var task int64
		var snapshot json.RawMessage
		if e := tx.QueryRowContext(ctx, "SELECT status,workspace,lease_expires>UTC_TIMESTAMP(6),task_id,input_snapshot FROM am_task_attempts WHERE id=? FOR UPDATE", attempt).Scan(&status, &workspace, &alive, &task, &snapshot); e != nil {
			return e
		}
		var role, previous string
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT role,round_number,COALESCE(submission_hash,'') FROM am_task_rounds WHERE id=? FOR UPDATE", id).Scan(&role, &n, &previous); e != nil {
			return e
		}
		hash := sha256.Sum256(payload)
		digest := hex.EncodeToString(hash[:])
		if previous != "" {
			if previous == digest && kind != "progress" {
				return nil
			}
			return ErrConflict
		}
		if !alive || (status != "claimed" && status != "running") {
			return ErrConflict
		}
		var latest string
		if e := tx.QueryRowContext(ctx, "SELECT id FROM am_task_rounds WHERE attempt_id=? ORDER BY round_number DESC,FIELD(role,'worker','reviewer') DESC LIMIT 1", attempt).Scan(&latest); e != nil {
			return e
		}
		if latest != id {
			return ErrConflict
		}
		if kind == "progress" {
			var p struct {
				Summary string `json:"summary"`
			}
			if e := json.Unmarshal(payload, &p); e != nil {
				return e
			}
			if strings.TrimSpace(p.Summary) == "" || len(p.Summary) > 4000 {
				return errors.New("progress requires 1..4000 characters")
			}
			return s.log(ctx, tx, task, attempt, role, "progress", p.Summary, nil)
		}
		var body any
		var summary string
		if role == "worker" && kind == "result" {
			var p Submission
			if e := json.Unmarshal(payload, &p); e != nil {
				return e
			}
			if p.Outcome != "completed" && p.Outcome != "blocked" && p.Outcome != "failed" {
				return errors.New("invalid outcome")
			}
			if strings.TrimSpace(p.Summary) == "" || len(p.Summary) > 16000 {
				return errors.New("result summary required (max 16000 characters)")
			}
			var snap Snapshot
			if e := json.Unmarshal(snapshot, &snap); e != nil {
				return e
			}
			if p.Outcome == "completed" {
				if e := requiredOutputs(workspace, snap); e != nil {
					return e
				}
				p.ArtifactPaths = append(p.ArtifactPaths, snap.Outputs...)
			}
			artifacts, e := archiveArtifacts(s.Config, workspace, p.ArtifactPaths)
			if e != nil {
				return e
			}
			p.Artifacts = artifacts
			p.ArtifactPaths = nil
			body = p
			summary = p.Summary
		} else if role == "reviewer" && kind == "review" {
			var d ReviewDecision
			if e := json.Unmarshal(payload, &d); e != nil {
				return e
			}
			var raw []byte
			if e := tx.QueryRowContext(ctx, "SELECT submission FROM am_task_rounds WHERE attempt_id=? AND round_number<=? AND role='worker' ORDER BY round_number DESC LIMIT 1", attempt, n).Scan(&raw); e != nil {
				return e
			}
			var p Submission
			if e := json.Unmarshal(raw, &p); e != nil {
				return e
			}
			if e := d.validate(p); e != nil {
				return e
			}
			body = d
			summary = d.Summary
		} else {
			return errors.New("tool is not permitted for this round role")
		}
		raw, e := json.Marshal(body)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE am_task_rounds SET submission=?,submission_hash=?,completed_at=UTC_TIMESTAMP(6) WHERE id=?", string(raw), digest, id); e != nil {
			return e
		}
		return s.log(ctx, tx, task, attempt, role, role+"_submitted", summary, body)
	})
}
