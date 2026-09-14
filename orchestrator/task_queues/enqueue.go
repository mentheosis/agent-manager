package taskqueues

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

type TaskSpec struct {
	Key         string          `json:"key"`
	Type        string          `json:"task_type"`
	Parameters  json.RawMessage `json:"parameters"`
	DependsOn   string          `json:"depends_on,omitempty"`
	DependsOnID int64           `json:"depends_on_id,omitempty"`
	Review      *bool           `json:"human_review_required,omitempty"`
	MaxAttempts int             `json:"max_attempts,omitempty"`
	Order       int             `json:"task_order,omitempty"`
	Priority    int             `json:"priority,omitempty"`
}
type Batch struct {
	Key         string     `json:"batch_key"`
	Workflow    string     `json:"workflow_id"`
	Tasks       []TaskSpec `json:"tasks"`
	PreviewHash string     `json:"preview_hash,omitempty"`
}
type RenderedTask struct {
	WorkingDirectory string     `json:"working_directory"`
	Isolated         bool       `json:"use_isolated_workspace"`
	Execution        Definition `json:"execution"`
	Key              string     `json:"key"`
	Type             string     `json:"task_type"`
	Prompt           string     `json:"prompt"`
	Inputs           []string   `json:"inputs"`
	Outputs          []string   `json:"outputs"`
	Warnings         []string   `json:"warnings"`
}
type Preview struct {
	QueueID string         `json:"queue_id"`
	Tasks   []RenderedTask `json:"tasks"`
	Hash    string         `json:"preview_hash"`
}

func RenderBatch(c Config, b Batch) (Preview, error) {
	out := Preview{QueueID: c.QueueID, Tasks: []RenderedTask{}}
	if !queueKey(b.Key) || !queueKey(b.Workflow) || len(b.Tasks) < 1 || len(b.Tasks) > 50 {
		return out, errors.New("batch_key/workflow_id must be 1–128 letters, digits, underscores or hyphens; batch needs 1–50 tasks")
	}
	seen := map[string]RenderedTask{}
	total := 0
	for _, spec := range b.Tasks {
		if !queueKey(spec.Key) || len(b.Key)+len(spec.Key) > 254 || spec.MaxAttempts < 0 || spec.MaxAttempts > 100 || spec.DependsOnID < 0 {
			return out, errors.New("invalid task key, attempt budget or dependency")
		}
		if _, ok := seen[spec.Key]; ok {
			return out, errors.New("duplicate task key")
		}
		if spec.DependsOn != "" && spec.DependsOnID != 0 {
			return out, errors.New("choose depends_on or depends_on_id")
		}
		if spec.DependsOn != "" {
			if _, ok := seen[spec.DependsOn]; !ok {
				return out, errors.New("depends_on must name an earlier task key in this batch")
			}
		}
		prepared, err := readDefinition(c, Task{Type: spec.Type, Parameters: spec.Parameters})
		if err != nil {
			return out, fmt.Errorf("task %s: %w", spec.Key, err)
		}
		row := RenderedTask{WorkingDirectory: prepared.Repository, Isolated: isolated(prepared.Config.UseIsolatedWorkspace), Execution: prepared.Definition, Key: spec.Key, Type: spec.Type, Prompt: assignment(string(prepared.Instructions), prepared.Inputs, prepared.Outputs), Inputs: prepared.Inputs, Outputs: prepared.Outputs, Warnings: []string{}}
		row.Prompt += "\n\nReport progress with queue_progress. Finish with queue_submit_result; completion requires every declared output file."
		for _, input := range row.Inputs {
			if strings.HasPrefix(input, "upstream:") {
				if spec.DependsOn == "" && spec.DependsOnID == 0 {
					return out, fmt.Errorf("task %s declares predecessor inputs but has no dependency", spec.Key)
				}
				if spec.DependsOn != "" {
					path := strings.TrimPrefix(input, "upstream:")
					found := false
					for _, output := range seen[spec.DependsOn].Outputs {
						if path == output {
							found = true
						}
					}
					if !found {
						return out, fmt.Errorf("task %s input %s is not a declared output of %s", spec.Key, input, spec.DependsOn)
					}
				}
				row.Warnings = append(row.Warnings, "Pending accepted predecessor artifact: "+input)
			} else if _, err := fileSignature(prepared.Repository, input); err != nil {
				row.Warnings = append(row.Warnings, "Input must be available before execution: "+input)
			}
		}
		if isolated(prepared.Config.UseIsolatedWorkspace) {
			row.Warnings = append(row.Warnings, "Isolated execution reads committed Git HEAD files; preview inspects the mounted repository.")
		}
		total += len(row.Prompt)
		if total > 4<<20 {
			return out, errors.New("rendered batch exceeds 4 MiB")
		}
		seen[spec.Key] = row
		out.Tasks = append(out.Tasks, row)
	}
	copyBatch := b
	copyBatch.PreviewHash = ""
	out.Hash = stableHash(map[string]any{"batch": copyBatch, "rendered": out})
	return out, nil
}
func queueKey(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func (s *Store) Enqueue(ctx context.Context, b Batch) (map[string]int64, error) {
	preview, err := RenderBatch(s.Config, b)
	if err != nil {
		return nil, err
	}
	if b.PreviewHash != "" && b.PreviewHash != preview.Hash {
		return nil, errors.New("preview changed; render the batch again before loading")
	}
	b.PreviewHash = ""
	digest := stableHash(b)
	ids := map[string]int64{}
	err = s.transaction(ctx, func(tx *Tx) error {
		for index, spec := range b.Tasks {
			var dependency any
			if spec.DependsOn != "" {
				dependency = ids[spec.DependsOn]
			} else if spec.DependsOnID != 0 {
				var id int64
				if err := tx.QueryRowContext(ctx, "SELECT id FROM am_tasks WHERE id=? AND queue_id=? AND workflow_id=?", spec.DependsOnID, s.Config.QueueID, b.Workflow).Scan(&id); err != nil {
					return errors.New("existing dependency must belong to this queue and workflow")
				}
				dependency = id
			}
			review := true
			if spec.Review != nil {
				review = *spec.Review
			}
			budget := spec.MaxAttempts
			if budget == 0 {
				budget = 3
			}
			key := b.Key + "/" + spec.Key
			if index == 0 {
				key = b.Key + "/"
			} // Stable first-row key serializes the whole batch identity.
			// The unique key serializes concurrent loads. Existing lifecycle/inputs are
			// never overwritten; changed batches conflict and roll back the whole load.
			if _, err := tx.ExecContext(ctx, `INSERT INTO am_tasks(queue_id,workflow_id,task_type,parameters,depends_on,human_review_required,max_attempts,task_order,priority,producer_key,request_hash) VALUES (?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE id=id`, s.Config.QueueID, b.Workflow, spec.Type, string(spec.Parameters), dependency, review, budget, spec.Order, spec.Priority, key, digest); err != nil {
				return err
			}
			var id int64
			var stored sql.NullString
			if err := tx.QueryRowContext(ctx, "SELECT id,request_hash FROM am_tasks WHERE queue_id=? AND workflow_id=? AND producer_key=? FOR UPDATE", s.Config.QueueID, b.Workflow, key).Scan(&id, &stored); err != nil {
				return err
			}
			if !stored.Valid || stored.String != digest {
				return fmt.Errorf("batch key %q already used with different content", b.Key)
			}
			ids[spec.Key] = id
		}
		return nil
	})
	return ids, err
}
func QueueCommand(ctx context.Context, c Config, action string, input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("batch exceeds 1 MiB")
	}
	var batch Batch
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&batch); err != nil {
		return fmt.Errorf("invalid batch: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected a single batch JSON object")
	}
	if action == "render" {
		result, err := RenderBatch(c, batch)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	if action != "enqueue" {
		return errors.New("unknown queue command")
	}
	store, err := Open(c)
	if err != nil {
		return err
	}
	defer store.DB.Close()
	if err = store.Check(ctx); err != nil {
		return err
	}
	ids, err := store.Enqueue(ctx, batch)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"queue_id": c.QueueID, "task_ids": ids})
}

func stableHash(value any) string {
	raw, _ := json.Marshal(value)
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&decoded)
	canonical, _ := json.Marshal(decoded)
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}
