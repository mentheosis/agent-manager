package taskqueues

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/go-sql-driver/mysql"
	"regexp"
	"strings"
	"time"
)

//go:embed schema/001.sql
var Schema string
var ErrConflict = errors.New("state changed, ownership expired, or command is not valid")

const active = "('claimed','running','submitted')"

type Store struct {
	DB       *Database
	Config   Config
	controls *controllerControls
}
type Task struct {
	ID           int64           `json:"id"`
	QueueID      string          `json:"queue_id"`
	Workflow     string          `json:"workflow_id"`
	Type         string          `json:"task_type"`
	Status       string          `json:"status"`
	Parameters   json.RawMessage `json:"parameters"`
	AttemptCount int             `json:"attempt_count"`
	MaxAttempts  int             `json:"max_attempts"`
	Review       bool            `json:"human_review_required"`
	Latest       string          `json:"latest_attempt"`
	Depends      sql.NullInt64   `json:"-"`
	Result       json.RawMessage `json:"accepted_result,omitempty"`
	LatestResult json.RawMessage `json:"latest_result,omitempty"`
	LatestError  string          `json:"latest_error,omitempty"`
	LatestUsage  json.RawMessage `json:"latest_usage,omitempty"`
}
type Attempt struct {
	ID           string          `json:"id"`
	TaskID       int64           `json:"task_id"`
	Owner        string          `json:"owner"`
	Status       string          `json:"status"`
	Conversation string          `json:"conversation_id"`
	Workspace    string          `json:"workspace"`
	Snapshot     json.RawMessage `json:"input_snapshot"`
	Submission   json.RawMessage `json:"submission,omitempty"`
	Started      time.Time       `json:"started_at"`
	Lease        time.Time       `json:"lease_expires"`
}
type Status struct {
	Limits     Limits `json:"limits"`
	QueueID    string `json:"queue_id"`
	MaxWorkers int    `json:"max_workers"`
	Paused     bool   `json:"paused"`
	Available  int    `json:"available_tasks"`
	Active     int    `json:"active_tasks"`
	Workers    int    `json:"active_workers"`
	Revision   int64  `json:"revision"`
}
type Log struct {
	ID        int64           `json:"id"`
	TaskID    *int64          `json:"task_id"`
	AttemptID *string         `json:"attempt_id"`
	Actor     string          `json:"actor"`
	Type      string          `json:"event_type"`
	Summary   string          `json:"summary"`
	Data      json.RawMessage `json:"data,omitempty"`
	Created   time.Time       `json:"created_at"`
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func Open(c Config) (*Store, error) {
	cfg, e := mysql.ParseDSN(c.DSN)
	if e != nil {
		return nil, errors.New("invalid database DSN")
	}
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 30 * time.Second
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	db, e := sql.Open("mysql", cfg.FormatDSN())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxLifetime(5 * time.Minute)
	if c.TablePrefix == "" {
		c.TablePrefix = "am_"
	}
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,40}$`).MatchString(c.TablePrefix) {
		db.Close()
		return nil, errors.New("invalid table prefix")
	}
	if c.ControllerID == "" {
		c.ControllerID = "owner"
	}
	controls, err := loadControls(c)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{DB: &Database{db, c.TablePrefix}, Config: c, controls: controls}, nil
}

func (s *Store) Init(ctx context.Context) error {
	var existing int
	if e := s.DB.QueryRowContext(ctx, "SELECT MAX(version) FROM am_schema_version").Scan(&existing); e == nil && existing != 1 {
		return errors.New("unsupported queue schema version; initialize a compatible schema explicitly")
	}
	for _, q := range strings.Split(Schema, ";") {
		if strings.TrimSpace(q) == "" {
			continue
		}
		if _, err := s.DB.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) Check(ctx context.Context) error {
	var version int
	if err := s.DB.QueryRowContext(ctx, "SELECT MAX(version) FROM am_schema_version").Scan(&version); err != nil {
		return errors.New("queue schema missing or inaccessible; apply schema explicitly")
	}
	if version != 1 {
		return errors.New("unsupported queue schema version")
	}
	return nil
}
func (s *Store) transaction(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) log(ctx context.Context, tx *Tx, task any, attempt any, actor, kind, summary string, data any) error {
	var raw any
	if data != nil {
		b, e := json.Marshal(data)
		if e != nil {
			return e
		}
		raw = string(b)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO am_work_log(queue_id,task_id,attempt_id,actor,event_type,summary,data) VALUES (?,?,?,?,?,?,?)", s.Config.QueueID, task, attempt, actor, kind, summary, raw)
	return err
}

const eligible = `t.queue_id=? AND t.status IN ('queued','retry_wait') AND t.available_at<=UTC_TIMESTAMP(6) AND t.attempt_count<t.max_attempts AND (t.depends_on IS NULL OR EXISTS(SELECT 1 FROM am_tasks d WHERE d.id=t.depends_on AND d.queue_id=t.queue_id AND d.workflow_id=t.workflow_id AND d.status='completed'))`

func (s *Store) Claim(ctx context.Context, owner string) (*Attempt, *Task, error) {
	if owner != s.Config.ControllerID {
		return nil, nil, errors.New("claim owner must match controller")
	}
	s.controls.mu.Lock()
	defer s.controls.mu.Unlock()
	var a *Attempt
	var task *Task
	err := s.transaction(ctx, func(tx *Tx) error {
		if s.controls.Paused {
			return nil
		}
		var n int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM am_task_attempts WHERE queue_id=? AND owner=? AND backend_id=? AND status IN "+active, s.Config.QueueID, owner, s.backendID()).Scan(&n); e != nil {
			return e
		}
		if n >= s.controls.MaxWorkers {
			return nil
		}
		t := Task{}
		var latest sql.NullString
		e := tx.QueryRowContext(ctx, `SELECT t.id,t.workflow_id,t.task_type,t.status,t.parameters,t.attempt_count,t.max_attempts,t.human_review_required,t.latest_attempt,t.depends_on FROM am_tasks t WHERE `+eligible+` ORDER BY t.priority DESC,t.task_order,t.id LIMIT 1 FOR UPDATE SKIP LOCKED`, s.Config.QueueID).Scan(&t.ID, &t.Workflow, &t.Type, &t.Status, &t.Parameters, &t.AttemptCount, &t.MaxAttempts, &t.Review, &latest, &t.Depends)
		if e == sql.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		initial, _ := json.Marshal(Snapshot{Limits: s.controls.Limits})
		var previous json.RawMessage
		err := tx.QueryRowContext(ctx, "SELECT input_snapshot FROM am_task_attempts WHERE task_id=? AND input_snapshot IS NOT NULL ORDER BY started_at DESC,id DESC LIMIT 1", t.ID).Scan(&previous)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if len(previous) > 0 && string(previous) != "null" {
			initial = previous
		}
		var snap Snapshot
		if err = json.Unmarshal(initial, &snap); err != nil {
			return err
		}
		a = &Attempt{ID: ID(), TaskID: t.ID, Owner: owner, Status: "claimed", Snapshot: initial}
		task = &t
		if _, e = tx.ExecContext(ctx, `INSERT INTO am_task_attempts(id,task_id,queue_id,owner,backend_id,lease_expires,input_snapshot) VALUES (?,?,?,?,?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)),?)`, a.ID, t.ID, s.Config.QueueID, owner, s.backendID(), snap.Limits.LeaseSeconds, string(initial)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE am_tasks SET status='running',latest_attempt=?,attempt_count=attempt_count+1,updated_at=UTC_TIMESTAMP(6) WHERE id=?`, a.ID, t.ID); e != nil {
			return e
		}
		return s.log(ctx, tx, t.ID, a.ID, owner, "claimed", "Worker slot reserved", nil)
	})
	return a, task, err
}
func (s *Store) Status(ctx context.Context) (out Status, err error) {
	s.controls.mu.Lock()
	defer s.controls.mu.Unlock()
	out.Limits = s.controls.Limits
	out.QueueID = s.Config.QueueID
	out.MaxWorkers = s.controls.MaxWorkers
	out.Paused = s.controls.Paused
	out.Revision = s.controls.Revision
	if err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM am_tasks t WHERE "+eligible, s.Config.QueueID).Scan(&out.Available); err != nil {
		return
	}
	err = s.DB.QueryRowContext(ctx, "SELECT COUNT(*),COUNT(DISTINCT task_id) FROM am_task_attempts WHERE queue_id=? AND owner=? AND backend_id=? AND status IN "+active, s.Config.QueueID, s.Config.ControllerID, s.backendID()).Scan(&out.Workers, &out.Active)
	return
}
func (s *Store) Configure(ctx context.Context, limit *int, paused *bool, actor string) error {
	if limit != nil && (*limit < 1) {
		return errors.New("max_workers outside configured range")
	}
	s.controls.mu.Lock()
	defer s.controls.mu.Unlock()
	old := s.controls.MaxWorkers
	next := old
	p := s.controls.Paused
	if limit != nil {
		next = *limit
	}
	if paused != nil {
		p = *paused
	}
	// Local controller state owns controls. A log write failure is surfaced without
	// acknowledging a change; configuration events identify the specific controller.
	if err := s.transaction(ctx, func(tx *Tx) error {
		return s.log(ctx, tx, nil, nil, actor, "configuration", "Controller controls requested", map[string]any{"controller_id": s.Config.ControllerID, "old_max_workers": old, "max_workers": next, "paused": p})
	}); err != nil {
		return err
	}
	return s.controls.save(next, p)
}
func (s *Store) backendID() string {
	hash := sha256.Sum256([]byte(s.Config.InternalToken))
	return hex.EncodeToString(hash[:])
}
func (s *Store) Attempts(ctx context.Context) ([]Attempt, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id,task_id,owner,status,COALESCE(conversation_id,''),COALESCE(workspace,''),COALESCE(input_snapshot,'null'),COALESCE(submission,'null'),started_at,lease_expires FROM am_task_attempts WHERE queue_id=? AND backend_id=? AND status IN "+active, s.Config.QueueID, s.backendID())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Attempt{}
	for rows.Next() {
		var a Attempt
		if e = rows.Scan(&a.ID, &a.TaskID, &a.Owner, &a.Status, &a.Conversation, &a.Workspace, &a.Snapshot, &a.Submission, &a.Started, &a.Lease); e != nil {
			return nil, e
		}
		if string(a.Submission) == "null" {
			a.Submission = nil
		}
		if string(a.Snapshot) == "null" {
			a.Snapshot = nil
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) Task(ctx context.Context, id int64) (Task, error) {
	var t Task
	var latest sql.NullString
	e := s.DB.QueryRowContext(ctx, "SELECT id,queue_id,workflow_id,task_type,status,parameters,attempt_count,max_attempts,human_review_required,latest_attempt,depends_on,COALESCE(accepted_result,'null') FROM am_tasks WHERE id=? AND queue_id=?", id, s.Config.QueueID).Scan(&t.ID, &t.QueueID, &t.Workflow, &t.Type, &t.Status, &t.Parameters, &t.AttemptCount, &t.MaxAttempts, &t.Review, &latest, &t.Depends, &t.Result)
	if string(t.Result) == "null" {
		t.Result = nil
	}
	t.Latest = latest.String
	if e == nil && t.Latest != "" {
		e = s.DB.QueryRowContext(ctx, "SELECT COALESCE(submission,'null'),COALESCE(error_text,''),COALESCE(resource_usage,'null') FROM am_task_attempts WHERE id=? AND queue_id=?", t.Latest, s.Config.QueueID).Scan(&t.LatestResult, &t.LatestError, &t.LatestUsage)
	}
	return t, e
}
func (s *Store) Prepare(ctx context.Context, a Attempt, workspace string, snapshot json.RawMessage) error {
	return s.transaction(ctx, func(tx *Tx) error {
		r, e := tx.ExecContext(ctx, "UPDATE am_task_attempts SET workspace=?,input_snapshot=? WHERE id=? AND queue_id=? AND owner=? AND status='claimed' AND lease_expires>UTC_TIMESTAMP(6)", workspace, string(snapshot), a.ID, s.Config.QueueID, a.Owner)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return nil
	})
}
func (s *Store) Launched(ctx context.Context, a Attempt, conversation string) error {
	return s.transaction(ctx, func(tx *Tx) error {
		r, e := tx.ExecContext(ctx, "UPDATE am_task_attempts SET conversation_id=?,status=IF(status='submitted','submitted','running') WHERE id=? AND queue_id=? AND owner=? AND status IN "+active+" AND lease_expires>UTC_TIMESTAMP(6)", conversation, a.ID, s.Config.QueueID, a.Owner)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return s.log(ctx, tx, a.TaskID, a.ID, a.Owner, "started", "Worker conversation started", map[string]string{"conversation_id": conversation})
	})
}
func (s *Store) Renew(ctx context.Context, a Attempt) error {
	r, e := s.DB.ExecContext(ctx, "UPDATE am_task_attempts SET lease_expires=TIMESTAMPADD(SECOND,COALESCE(JSON_EXTRACT(input_snapshot,'$.limits.lease_secs'),?),UTC_TIMESTAMP(6)),heartbeat_at=UTC_TIMESTAMP(6) WHERE id=? AND queue_id=? AND owner=? AND status IN "+active+" AND lease_expires>UTC_TIMESTAMP(6)", s.Config.LeaseSeconds, a.ID, s.Config.QueueID, a.Owner)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}

// Only called after execution has ended or a backend cancellation is acknowledged.
func (s *Store) Finish(ctx context.Context, a Attempt, outcome, reason string) error {
	return s.transaction(ctx, func(tx *Tx) error {
		var review bool
		var count, budget int
		var latest string
		if e := tx.QueryRowContext(ctx, "SELECT human_review_required,attempt_count,max_attempts,COALESCE(latest_attempt,'') FROM am_tasks WHERE id=? AND queue_id=? FOR UPDATE", a.TaskID, s.Config.QueueID).Scan(&review, &count, &budget, &latest); e != nil {
			return e
		}
		if latest != a.ID {
			return ErrConflict
		}
		var status string
		var submission []byte
		if e := tx.QueryRowContext(ctx, "SELECT status,submission FROM am_task_attempts WHERE id=? AND queue_id=? AND backend_id=? FOR UPDATE", a.ID, s.Config.QueueID, s.backendID()).Scan(&status, &submission); e != nil {
			return e
		}
		if status != "claimed" && status != "running" && status != "submitted" {
			return nil
		}
		var result any
		next := outcome
		if outcome == "ended" && len(submission) == 0 {
			outcome = "failed"
			next = "failed"
			reason = "Worker ended without a structured result"
		}
		if outcome == "submitted" || (outcome == "ended" && len(submission) > 0) {
			var body struct {
				Outcome string `json:"outcome"`
			}
			if e := json.Unmarshal(submission, &body); e != nil {
				return e
			}
			next = body.Outcome
			if outcome == "ended" {
				reason = "Submitted outcome recorded"
			}
			result = string(submission)
			if next == "completed" && review {
				next = "awaiting_review"
			}
		}
		attemptState := next
		if next == "failed" && count < budget {
			next = "retry_wait"
		}
		if attemptState == "retry_wait" {
			attemptState = "failed"
		}
		if _, e := tx.ExecContext(ctx, "UPDATE am_task_attempts SET status=?,error_text=?,completed_at=UTC_TIMESTAMP(6) WHERE id=?", attemptState, reason, a.ID); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, "UPDATE am_tasks SET status=?,accepted_result=?,available_at=TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)),updated_at=UTC_TIMESTAMP(6) WHERE id=?", next, func() any {
			if next == "completed" {
				return result
			}
			return nil
		}(), min(300, 5*count), a.TaskID); e != nil {
			return e
		}
		return s.log(ctx, tx, a.TaskID, a.ID, a.Owner, next, reason, map[string]any{"submission": json.RawMessage(submission)})
	})
}
func (s *Store) Tasks(ctx context.Context, offset int) ([]Task, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id FROM am_tasks WHERE queue_id=? ORDER BY id DESC LIMIT 100 OFFSET ?", s.Config.QueueID, offset)
	if e != nil {
		return nil, e
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return nil, e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	out := []Task{}
	for _, id := range ids {
		t, e := s.Task(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, nil
}
func (s *Store) Logs(ctx context.Context, after int64) ([]Log, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT id,task_id,attempt_id,actor,event_type,summary,COALESCE(data,'null'),created_at FROM am_work_log WHERE queue_id=? AND id>? ORDER BY id LIMIT 200", s.Config.QueueID, after)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Log{}
	for rows.Next() {
		var l Log
		if e = rows.Scan(&l.ID, &l.TaskID, &l.AttemptID, &l.Actor, &l.Type, &l.Summary, &l.Data, &l.Created); e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s *Store) Action(ctx context.Context, id int64, action, attemptID, actor string) error {
	return s.transaction(ctx, func(tx *Tx) error {
		var status, latest string
		if e := tx.QueryRowContext(ctx, "SELECT status,COALESCE(latest_attempt,'') FROM am_tasks WHERE id=? AND queue_id=? FOR UPDATE", id, s.Config.QueueID).Scan(&status, &latest); e != nil {
			return e
		}
		next := ""
		var accepted any
		switch action {
		case "approve", "reject":
			if status != "awaiting_review" || latest != attemptID {
				return ErrConflict
			}
			if action == "approve" {
				next = "completed"
				var data []byte
				if e := tx.QueryRowContext(ctx, "SELECT submission FROM am_task_attempts WHERE id=?", latest).Scan(&data); e != nil {
					return e
				}
				accepted = string(data)
			} else {
				next = "failed"
			}
			if _, e := tx.ExecContext(ctx, "UPDATE am_task_attempts SET status=? WHERE id=? AND status='awaiting_review'", next, latest); e != nil {
				return e
			}
		case "retry", "unblock":
			if status != "failed" && status != "blocked" {
				return ErrConflict
			}
			var count, max int
			if e := tx.QueryRowContext(ctx, "SELECT attempt_count,max_attempts FROM am_tasks WHERE id=?", id).Scan(&count, &max); e != nil {
				return e
			}
			if count >= max {
				return errors.New("attempt budget exhausted")
			}
			next = "queued"
		case "cancel":
			if status == "running" {
				return errors.New("cancel active attempt through scheduler")
			}
			if status == "completed" {
				return ErrConflict
			}
			next = "cancelled"
		default:
			return errors.New("unknown action")
		}
		if _, e := tx.ExecContext(ctx, "UPDATE am_tasks SET status=?,accepted_result=?,available_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?", next, accepted, id); e != nil {
			return e
		}
		return s.log(ctx, tx, id, func() any {
			if latest == "" {
				return nil
			}
			return latest
		}(), actor, action, "Task "+action, nil)
	})
}
func (s *Store) ValidateDependencies(ctx context.Context) error {
	rows, e := s.DB.QueryContext(ctx, "SELECT id,depends_on,workflow_id FROM am_tasks WHERE queue_id=?", s.Config.QueueID)
	if e != nil {
		return e
	}
	type dep struct {
		id       sql.NullInt64
		workflow string
	}
	deps := map[int64]dep{}
	for rows.Next() {
		var id int64
		var d dep
		if e = rows.Scan(&id, &d.id, &d.workflow); e != nil {
			rows.Close()
			return e
		}
		deps[id] = d
	}
	rows.Close()
	for id, d := range deps {
		seen := map[int64]bool{id: true}
		valid := true
		for d.id.Valid {
			p, ok := deps[d.id.Int64]
			if !ok || p.workflow != d.workflow || seen[d.id.Int64] {
				valid = false
				break
			}
			seen[d.id.Int64] = true
			d = p
		}
		if !valid {
			e = s.transaction(ctx, func(tx *Tx) error {
				r, e := tx.ExecContext(ctx, "UPDATE am_tasks SET status='blocked' WHERE id=? AND status IN ('queued','retry_wait')", id)
				if e != nil {
					return e
				}
				n, _ := r.RowsAffected()
				if n > 0 {
					return s.log(ctx, tx, id, nil, "scheduler", "blocked", "Invalid dependency: cycle, missing task or cross-workflow reference", nil)
				}
				return nil
			})
			if e != nil {
				return e
			}
		}
	}
	return nil
}

func (s *Store) ConfigureLimits(ctx context.Context, limits Limits, actor string) error {
	if err := limits.validate(); err != nil {
		return err
	}
	s.controls.mu.Lock()
	defer s.controls.mu.Unlock()
	if err := s.transaction(ctx, func(tx *Tx) error {
		return s.log(ctx, tx, nil, nil, actor, "configuration", "Controller attempt limits requested", map[string]any{"controller_id": s.Config.ControllerID, "limits": limits})
	}); err != nil {
		return err
	}
	return s.controls.save(s.controls.MaxWorkers, s.controls.Paused, limits)
}
