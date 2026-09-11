package taskqueues

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

type Submission struct {
	Outcome       string     `json:"outcome"`
	Summary       string     `json:"summary"`
	ArtifactPaths []string   `json:"artifact_paths,omitempty"`
	Artifacts     []Artifact `json:"artifacts,omitempty"`
	Blockers      []string   `json:"blockers,omitempty"`
}

func (s *Store) Submit(ctx context.Context, id, token, kind string, payload json.RawMessage) error {
	if !hmac.Equal([]byte(capability(s.Config.InternalToken, id)), []byte(token)) {
		return errors.New("invalid attempt capability")
	}
	if len(payload) > 1<<20 {
		return errors.New("submission too large")
	}
	return s.transaction(ctx, func(tx *Tx) error {
		var task int64
		var status, workspace string
		var previous sql.NullString
		var alive bool
		if e := tx.QueryRowContext(ctx, "SELECT task_id,status,COALESCE(workspace,''),submission_hash,lease_expires>UTC_TIMESTAMP(6) FROM am_task_attempts WHERE id=? AND queue_id=? FOR UPDATE", id, s.Config.QueueID).Scan(&task, &status, &workspace, &previous, &alive); e != nil {
			return ErrConflict
		}
		hash := sha256.Sum256(payload)
		digest := hex.EncodeToString(hash[:])
		if kind == "result" && previous.Valid {
			if previous.String == digest {
				return nil
			}
			return ErrConflict
		}
		if !alive || (status != "claimed" && status != "running") {
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
				return errors.New("progress requires a summary of 1–4000 characters")
			}
			return s.log(ctx, tx, task, id, "worker", "progress", p.Summary, nil)
		}
		if kind != "result" {
			return errors.New("unknown submission kind")
		}
		var p Submission
		if e := json.Unmarshal(payload, &p); e != nil {
			return e
		}
		if p.Outcome != "completed" && p.Outcome != "blocked" && p.Outcome != "failed" {
			return errors.New("invalid outcome")
		}
		if strings.TrimSpace(p.Summary) == "" || len(p.Summary) > 16000 {
			return errors.New("result summary is required (max 16000 characters)")
		}
		if workspace == "" {
			return ErrConflict
		}
		artifacts, e := archiveArtifacts(s.Config, workspace, p.ArtifactPaths)
		if e != nil {
			return e
		}
		p.Artifacts = artifacts
		p.ArtifactPaths = nil
		raw, e := json.Marshal(p)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE am_task_attempts SET status='submitted',submission=?,submission_hash=? WHERE id=?", string(raw), digest, id); e != nil {
			return e
		}
		return s.log(ctx, tx, task, id, "worker", "submitted", p.Summary, p)
	})
}
