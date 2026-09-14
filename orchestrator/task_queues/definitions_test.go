package taskqueues

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

func definitionFixture(t *testing.T) (Config, Task) {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) string {
		out, e := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if e != nil {
			t.Fatalf("git fixture: %s %v", out, e)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	os.WriteFile(filepath.Join(repo, "task.md"), []byte("Write a concise report about {{topic}} in report.md. Use queue_progress and queue_submit_result."), 0600)
	run("add", ".")
	run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")

	return Config{Repositories: map[string]string{"fixture": repo}, Tasks: map[string]Definition{"neutral": {Instructions: filepath.Join(repo, "task.md"), Provider: "codex", Model: "test", Permission: "workspace-write", Parameters: json.RawMessage(`{"topic":{"type":"string","required":true}}`)}}, WorkspaceRoot: t.TempDir(), LeaseSeconds: 60, MaxAttemptSeconds: 3600, MaxAttemptTokens: 2000000}, Task{Type: "neutral", Parameters: json.RawMessage(`{"topic":"example"}`)}
}
func TestPinnedDefinitionsAndParameterValidation(t *testing.T) {
	c, task := definitionFixture(t)
	c.QueueID = "fixture"
	c.ProfilePath = filepath.Join(t.TempDir(), "profiles.json")
	writeProfile := func() {
		raw, err := json.Marshal(map[string]any{c.QueueID: map[string]any{"tasks": c.Tasks, "repositories": c.Repositories}})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(c.ProfilePath, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile()
	workspace, snapshot, err := Resolve(context.Background(), c, task, ID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(workspace, "task.md"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(c.Repositories["fixture"], "task.md"), []byte("CHANGED INSTRUCTIONS"), 0600)
	def := c.Tasks["neutral"]
	def.Model = "changed-model"
	c.Tasks["neutral"] = def
	writeProfile()
	_, replay, err := resolveSnapshot(context.Background(), c, task, ID(), nil, snapshot)
	if err != nil || !bytes.Equal(snapshot, replay) {
		t.Fatal("retry changed snapshot", err)
	}
	if strings.Contains(string(replay), "CHANGED INSTRUCTIONS") || strings.Contains(string(replay), "changed-model") {
		t.Fatal("retry used new config")
	}
	freshWorkspace, fresh, err := Resolve(context.Background(), c, task, ID(), nil)
	if err != nil || !strings.Contains(string(fresh), "CHANGED INSTRUCTIONS") {
		t.Fatal("new task did not use updated instructions", err)
	}
	freshInstructions, err := os.ReadFile(filepath.Join(freshWorkspace, "task.md"))
	if err != nil || string(freshInstructions) != "CHANGED INSTRUCTIONS" {
		t.Fatal("instruction overlay differs from prompt", err)
	}
	if len(original) == 0 {
		t.Fatal("missing source export")
	}
	task.Parameters = json.RawMessage(`{"unexpected":42}`)
	if _, _, err = Resolve(context.Background(), c, task, ID(), nil); err == nil {
		t.Fatal("accepted invalid parameters")
	}
	task.Type = "unknown"
	if _, _, err = Resolve(context.Background(), c, task, ID(), nil); err == nil {
		t.Fatal("accepted unknown task type")
	}
	if err = os.RemoveAll(c.Repositories["fixture"]); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(c.ProfilePath); err != nil {
		t.Fatal(err)
	}
	if _, replayed, err := resolveSnapshot(context.Background(), c, task, ID(), nil, snapshot); err != nil || !bytes.Equal(replayed, snapshot) {
		t.Fatal("saved retry depended on mutable repository/profile", err)
	}

}

func TestArchiveAndArtifactContainment(t *testing.T) {
	for _, h := range []*tar.Header{{Name: "../escape", Mode: 0600, Typeflag: tar.TypeReg}, {Name: "link", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		tw.WriteHeader(h)
		tw.Close()
		if unpack(&b, t.TempDir()) == nil {
			t.Fatal("accepted unsafe archive")
		}
	}
	c := Config{WorkspaceRoot: t.TempDir()}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "report.md"), []byte("original report"), 0600)
	artifacts, e := archiveArtifacts(c, ws, []string{"report.md"})
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(ws, "report.md"), []byte("mutated report"), 0600)
	data, e := os.ReadFile(filepath.Join(c.WorkspaceRoot, "artifacts", artifacts[0].SHA256))
	if e != nil || string(data) != "original report" {
		t.Fatal("artifact was mutable", e)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(ws, "link"))
	if _, e = archiveArtifacts(c, ws, []string{"link"}); e == nil {
		t.Fatal("artifact escaped workspace")
	}
	if _, e = archiveArtifacts(c, ws, []string{"../outside"}); e == nil {
		t.Fatal("accepted traversal")
	}
}

func TestSuccessorReceivesVerifiedPredecessorFiles(t *testing.T) {
	c, task := definitionFixture(t)
	prior := t.TempDir()
	os.WriteFile(filepath.Join(prior, "evidence.md"), []byte("Accepted evidence"), 0600)
	artifacts, e := archiveArtifacts(c, prior, []string{"evidence.md", "evidence.md"})
	if e != nil {
		t.Fatal(e)
	}
	if len(artifacts) != 1 {
		t.Fatal("duplicate artifact references were retained")
	}
	upstream, _ := json.Marshal(Submission{Outcome: "completed", Summary: "Evidence ready", Artifacts: artifacts})
	workspace, _, e := Resolve(context.Background(), c, task, ID(), upstream)
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(workspace, ".queue-inputs", "evidence.md"))
	if e != nil || string(data) != "Accepted evidence" {
		t.Fatal("predecessor evidence unavailable", e)
	}
	file := filepath.Join(c.WorkspaceRoot, "artifacts", artifacts[0].SHA256)
	os.Chmod(file, 0600)
	os.WriteFile(file, []byte("tampered"), 0600)
	if _, _, e = Resolve(context.Background(), c, task, ID(), upstream); e == nil {
		t.Fatal("accepted tampered evidence")
	}
}

func TestSimplifiedParameterMap(t *testing.T) {
	raw, err := compileParameterMap(json.RawMessage(`{"symbol":{"type":"string","required":true,"minLength":1},"limit":{"type":"integer","minimum":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("params.json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("params.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input string
		valid bool
	}{
		{`{"symbol":"TRX"}`, true}, {`{"symbol":"TRX","limit":2}`, true},
		{`{}`, false}, {`{"symbol":42}`, false}, {`{"symbol":""}`, false},
		{`{"symbol":"TRX","symbl":"LPT"}`, false}, {`{"symbol":"TRX","limit":0}`, false},
		{`{"symbol":"TRX","limit":1.5}`, false}, {`[]`, false}, {`null`, false}, {`"TRX"`, false},
	} {
		t.Run(tc.input, func(t *testing.T) {
			var input any
			if err := json.Unmarshal([]byte(tc.input), &input); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(input); (err == nil) != tc.valid {
				t.Fatalf("validation = %v, want valid %v", err, tc.valid)
			}
		})
	}
	for _, input := range []string{`null`, `[]`, `{"symbol":null}`, `{"symbol":{}}`, `{"symbol":{"type":"typo"}}`, `{"symbol":{"type":"string","required":"true"}}`} {
		if _, err := compileParameterMap(json.RawMessage(input)); err == nil {
			t.Errorf("accepted malformed definition %s", input)
		}
	}
	raw, err = compileParameterMap(json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	compiler = jsonschema.NewCompiler()
	if err = compiler.AddResource("empty.json", bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err = compiler.Compile("empty.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err = schema.Validate(map[string]any{"extra": true}); err == nil {
		t.Fatal("accepted undeclared input for parameterless task")
	}
}

func TestSharedWorkspacePreservesCheckoutAndSeparatesInputs(t *testing.T) {
	c, task := definitionFixture(t)
	flag := false
	c.UseIsolatedWorkspace = &flag
	repo := c.Repositories["fixture"]
	// Shared execution must work before the first commit, even without Git metadata.
	if err := os.RemoveAll(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("local data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(repo, ".queue-inputs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".queue-inputs", "untouched"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	evidence := t.TempDir()
	if err := os.WriteFile(filepath.Join(evidence, "report.md"), []byte("accepted evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	artifacts, err := archiveArtifacts(c, evidence, []string{"report.md"})
	if err != nil {
		t.Fatal(err)
	}
	upstream, _ := json.Marshal(Submission{Outcome: "completed", Summary: "evidence", Artifacts: artifacts})
	firstID := ID()
	workspace, raw, err := Resolve(context.Background(), c, task, firstID, upstream)
	if err != nil || workspace != repo {
		t.Fatal(workspace, err)
	}
	var first Snapshot
	if err = json.Unmarshal(raw, &first); err != nil {
		t.Fatal(err)
	}
	if isolated(first.UseIsolatedWorkspace) || first.SourceHash != "" {
		t.Fatal("shared mode archived sources")
	}
	got, err := os.ReadFile(filepath.Join(first.InputsDirectory, "report.md"))
	if err != nil || string(got) != "accepted evidence" {
		t.Fatal("missing inputs", err)
	}
	if !strings.Contains(first.Prompt, first.InputsDirectory) {
		t.Fatal("input location missing from prompt")
	}
	if _, err = os.Stat(filepath.Join(c.WorkspaceRoot, "sources")); !os.IsNotExist(err) {
		t.Fatal("shared mode created source archives")
	}
	if err = os.WriteFile(filepath.Join(repo, "task.md"), []byte("edited instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("updated data"), 0600); err != nil {
		t.Fatal(err)
	}
	// A retry retains its original mode/instructions even if the profile default changes.
	flag = true
	_, retryRaw, err := resolveSnapshot(context.Background(), c, task, ID(), nil, raw)
	if err != nil {
		t.Fatal(err)
	}
	var retry Snapshot
	if err = json.Unmarshal(retryRaw, &retry); err != nil {
		t.Fatal(err)
	}
	if retry.Instructions != first.Instructions || retry.BasePrompt != first.BasePrompt || retry.InputsDirectory == first.InputsDirectory {
		t.Fatal("retry lost original inputs or reused evidence directory")
	}
	for file, want := range map[string]string{"task.md": "edited instructions", "untracked.txt": "updated data", ".queue-inputs/untouched": "keep"} {
		data, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil || string(data) != want {
			t.Fatal("modified user checkout", file, err)
		}
	}
	if _, err = archiveArtifacts(c, workspace, []string{"untracked.txt"}); err != nil {
		t.Fatal("shared output not archivable", err)
	}
}
