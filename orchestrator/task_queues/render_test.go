package taskqueues

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestRenderUsesLiteralParametersAndFileContracts(t *testing.T) {
	c, task := definitionFixture(t)
	def := c.Tasks[task.Type]
	def.Inputs = []string{"task.md"}
	def.Outputs = []string{"out/{{topic}}.md"}
	c.Tasks[task.Type] = def
	os.WriteFile(def.Instructions, []byte("Investigate {{topic}}. Produce the listed output."), 0600)
	batch := Batch{Key: "test", Workflow: "run", Tasks: []TaskSpec{{Key: "one", Type: task.Type, Parameters: task.Parameters}}}
	preview, err := RenderBatch(c, batch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.Tasks[0].Prompt, "Investigate example") || preview.Tasks[0].Outputs[0] != "out/example.md" {
		t.Fatal(preview)
	}
	if _, err = os.Stat(filepath.Join(c.WorkspaceRoot, "attempts")); !os.IsNotExist(err) {
		t.Fatal("render created execution workspace")
	}
	for _, text := range []string{"{{missing}}", "{{topic | shell}}"} {
		if _, err = renderText(text, map[string]any{"topic": "ok"}); err == nil {
			t.Fatal("accepted invalid placeholder")
		}
	}
	value := "$(echo hello) {{not_evaluated}}"
	if rendered, err := renderText("{{topic}}", map[string]any{"topic": value}); err != nil || rendered != value {
		t.Fatal("template evaluated parameter content", rendered, err)
	}
	batch.Tasks[0].Parameters = json.RawMessage(`{"topic":"../../escape"}`)
	if _, err = RenderBatch(c, batch); err == nil {
		t.Fatal("accepted output traversal")
	}
}
func TestMissingDeclaredInputBlocksPreparation(t *testing.T) {
	c, task := definitionFixture(t)
	def := c.Tasks[task.Type]
	def.Inputs = []string{"missing.txt"}
	c.Tasks[task.Type] = def
	if _, _, err := Resolve(context.Background(), c, task, ID(), nil); err == nil || !strings.Contains(err.Error(), "required input") {
		t.Fatal(err)
	}
}
func TestDeclaredOutputsAreRequiredFreshAndAutomaticallyArchived(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	c, task := definitionFixture(t)
	shared := false
	s.Config.UseIsolatedWorkspace = &shared
	s.Config.Repositories = c.Repositories
	def := c.Tasks[task.Type]
	def.Outputs = []string{"report.md"}
	s.Config.Tasks = map[string]Definition{task.Type: def}
	os.WriteFile(filepath.Join(c.Repositories["fixture"], "report.md"), []byte("stale"), 0600)
	id := enqueue(t, s, false, nil)
	s.DB.ExecContext(ctx, "UPDATE am_tasks SET parameters=? WHERE id=?", string(task.Parameters), id)
	resume(t, s, 1)
	a := claim(t, s)
	workspace, snapshot, err := resolveSnapshot(ctx, s.Config, task, a.ID, nil, a.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Prepare(ctx, *a, workspace, snapshot); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"outcome":"completed","summary":"done"}`)
	token := capability(s.Config.InternalToken, a.ID)
	if err = s.Submit(ctx, a.ID, token, "result", payload); err == nil {
		t.Fatal("accepted stale pre-existing output")
	}
	os.Remove(filepath.Join(workspace, "report.md"))
	if err = s.Submit(ctx, a.ID, token, "result", payload); err == nil {
		t.Fatal("accepted missing output")
	}
	os.WriteFile(filepath.Join(workspace, "report.md"), []byte("new verified report"), 0600)
	if err = s.Submit(ctx, a.ID, token, "result", payload); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = s.DB.QueryRowContext(ctx, "SELECT submission FROM am_task_attempts WHERE id=?", a.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "report.md") || !strings.Contains(stored, "sha256") {
		t.Fatal("required output not archived", stored)
	}
	if err = s.Submit(ctx, a.ID, token, "result", payload); err != nil {
		t.Fatal("repeat submission not idempotent", err)
	}
}
func TestEnqueueBatchAtomicIdempotentAndDependencyChecked(t *testing.T) {
	s := testStore(t)
	c, task := definitionFixture(t)
	s.Config.Tasks = c.Tasks
	s.Config.Repositories = c.Repositories
	b := Batch{Key: "batch", Workflow: "run", Tasks: []TaskSpec{{Key: "one", Type: task.Type, Parameters: task.Parameters}, {Key: "two", Type: task.Type, Parameters: task.Parameters, DependsOn: "one"}}}
	ctx := context.Background()
	ids, err := s.Enqueue(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Enqueue(ctx, b)
			if err != nil || got["one"] != ids["one"] || got["two"] != ids["two"] {
				t.Errorf("duplicate batch: %v %v", got, err)
			}
		}()
	}
	wg.Wait()
	var count int
	s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM am_tasks").Scan(&count)
	if count != 2 {
		t.Fatal("duplicate tasks", count)
	}
	bad := b
	bad.Tasks = append([]TaskSpec(nil), b.Tasks...)
	bad.Tasks[0].Key = "different"
	bad.Tasks[1].DependsOn = "different"
	if _, err = s.Enqueue(ctx, bad); err == nil {
		t.Fatal("reused batch identity with different task keys")
	}
	bad = b
	bad.Key = "rollback"
	bad.Tasks = append([]TaskSpec(nil), b.Tasks...)
	bad.Tasks[1].DependsOn = ""
	bad.Tasks[1].DependsOnID = 999999999
	if _, err = s.Enqueue(ctx, bad); err == nil {
		t.Fatal("accepted foreign/missing dependency")
	}
	s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM am_tasks").Scan(&count)
	if count != 2 {
		t.Fatal("partial batch survived rollback", count)
	}
	preview, err := RenderBatch(s.Config, b)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.Tasks[task.Type].Instructions, []byte("Changed template"), 0600)
	b.PreviewHash = preview.Hash
	if _, err = s.Enqueue(ctx, b); err == nil {
		t.Fatal("loaded stale preview")
	}
}

func TestRenderingPreservesLargeIntegerParameters(t *testing.T) {
	c, _ := definitionFixture(t)
	def := c.Tasks["neutral"]
	def.Parameters = json.RawMessage(`{"height":{"type":"integer","required":true}}`)
	c.Tasks["neutral"] = def
	os.WriteFile(def.Instructions, []byte("Inspect {{height}}."), 0600)
	batch := Batch{Key: "number", Workflow: "run", Tasks: []TaskSpec{{Key: "one", Type: "neutral", Parameters: json.RawMessage(`{"height":9007199254740993}`)}}}
	preview, err := RenderBatch(c, batch)
	if err != nil || !strings.Contains(preview.Tasks[0].Prompt, "9007199254740993") {
		t.Fatal(preview, err)
	}
	if stableHash(json.RawMessage(`{"a":1,"b":2}`)) != stableHash(json.RawMessage(`{"b":2,"a":1}`)) {
		t.Fatal("hash depends on JSON object key order")
	}
}
