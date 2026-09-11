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
	os.WriteFile(filepath.Join(repo, "task.json"), []byte(`{"instructions":"task.md","parameters_schema":{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"],"additionalProperties":false}}`), 0600)
	os.WriteFile(filepath.Join(repo, "task.md"), []byte("Write a concise neutral report in report.md. Use queue_progress and queue_submit_result."), 0600)
	run("add", ".")
	run("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture")
	ref, _ := json.Marshal(DefinitionRef{"fixture", run("rev-parse", "HEAD"), "task.json"})
	return Config{Repositories: map[string]string{"fixture": repo}, WorkspaceRoot: t.TempDir()}, Task{Definition: ref, Parameters: json.RawMessage(`{"topic":"example"}`)}
}
func TestPinnedDefinitionsAndParameterValidation(t *testing.T) {
	c, task := definitionFixture(t)
	os.WriteFile(filepath.Join(c.Repositories["fixture"], "task.md"), []byte("MUTATED WORKING TREE"), 0600)
	id := ID()
	workspace, snapshot, e := Resolve(context.Background(), c, task, id, nil)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(snapshot), "MUTATED") {
		t.Fatal("used mutable instructions")
	}
	if _, e = os.Stat(filepath.Join(workspace, "task.md")); e != nil {
		t.Fatal(e)
	}
	// Simulate an interrupted export before SQL preparation and recover it.
	if _, _, e = Resolve(context.Background(), c, task, id, nil); e != nil {
		t.Fatal(e)
	}
	task.Parameters = json.RawMessage(`{"unexpected":42}`)
	if _, _, e = Resolve(context.Background(), c, task, ID(), nil); e == nil {
		t.Fatal("accepted invalid parameters")
	}
	task.Parameters = json.RawMessage(`{"topic":"example"}`)
	task.Definition = json.RawMessage(`{"repository":"fixture","commit":"HEAD","path":"task.json"}`)
	if _, _, e = Resolve(context.Background(), c, task, ID(), nil); e == nil {
		t.Fatal("accepted mutable ref")
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
