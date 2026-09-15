package taskqueues

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

type Definition struct {
	ReplayProfile string          `json:"replay_profile,omitempty"`
	Instructions  string          `json:"instructions"`
	Provider      string          `json:"provider"`
	Model         string          `json:"model,omitempty"`
	Permission    string          `json:"permission"`
	Parameters    json.RawMessage `json:"parameters"`
	Inputs        []string        `json:"inputs,omitempty"`
	Outputs       []string        `json:"outputs,omitempty"`
}

type Limits struct {
	LeaseSeconds int   `json:"lease_secs"`
	TaskSeconds  int   `json:"task_limit_secs"`
	TaskTokens   int64 `json:"task_limit_tokens"`
}

func (c Config) limits() Limits {
	return Limits{c.LeaseSeconds, c.MaxAttemptSeconds, c.MaxAttemptTokens}
}
func (l Limits) validate() error {
	if l.LeaseSeconds < 15 || l.TaskSeconds < l.LeaseSeconds || l.TaskTokens < 1 {
		return errors.New("invalid limits: lease >= 15, task time >= lease, tokens > 0 required")
	}
	return nil
}

// Task inputs are always a closed object. Each parameter uses a JSON Schema
// value definition, with an optional boolean required flag (default false).
func compileParameterMap(raw json.RawMessage) ([]byte, error) {
	var properties map[string]map[string]any
	if err := json.Unmarshal(raw, &properties); err != nil || properties == nil {
		return nil, errors.New("parameters must be an object of parameter definitions; use {} for no parameters")
	}
	required := []string{}
	for name, definition := range properties {
		if definition == nil {
			return nil, fmt.Errorf("parameter %q must have a definition", name)
		}
		kind, ok := definition["type"].(string)
		if !ok || !map[string]bool{"string": true, "number": true, "integer": true, "boolean": true, "array": true, "object": true, "null": true}[kind] {
			return nil, fmt.Errorf("parameter %q requires a valid type", name)
		}
		if value, exists := definition["required"]; exists {
			flag, ok := value.(bool)
			if !ok {
				return nil, fmt.Errorf("parameter %q required must be boolean", name)
			}
			if flag {
				required = append(required, name)
			}
			delete(definition, "required")
		}
	}
	return json.Marshal(map[string]any{"type": "object", "properties": properties,
		"required": required, "additionalProperties": false})
}

type Snapshot struct {
	InputSignatures      map[string]string `json:"input_signatures,omitempty"`
	Inputs               []string          `json:"inputs,omitempty"`
	Outputs              []string          `json:"outputs,omitempty"`
	OutputBaseline       map[string]string `json:"output_baseline,omitempty"`
	UseIsolatedWorkspace *bool             `json:"use_isolated_workspace,omitempty"`
	RepositoryPath       string            `json:"repository_path,omitempty"`
	InputsDirectory      string            `json:"inputs_directory,omitempty"`
	BasePrompt           string            `json:"base_prompt,omitempty"`
	InstructionPath      string            `json:"instruction_path"`
	TaskType             string            `json:"task_type"`
	Task                 Definition        `json:"task"`
	Instructions         string            `json:"instructions"`
	DefinitionHash       string            `json:"definition_hash"`
	Repository           string            `json:"repository"`
	Commit               string            `json:"commit"`
	SourceHash           string            `json:"source_hash"`
	Parameters           json.RawMessage   `json:"parameters"`
	Upstream             json.RawMessage   `json:"upstream,omitempty"`
	Prompt               string            `json:"prompt"`
	Limits               Limits            `json:"limits"`
}

func safeRelative(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, "\\")
}

// A deployment-approved mount can have a different host UID. Trust only this
// configured repository for this command; do not modify global Git settings.
func gitCommand(ctx context.Context, repo string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=" + repo, "-C", repo}, args...)...)
}
func Resolve(ctx context.Context, c Config, t Task, attempt string, upstream json.RawMessage) (string, json.RawMessage, error) {
	return resolveSnapshot(ctx, c, t, attempt, upstream, nil)
}

func resolveSnapshot(ctx context.Context, c Config, t Task, attempt string, upstream, saved json.RawMessage) (string, json.RawMessage, error) {
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(attempt) {
		return "", nil, errors.New("invalid attempt ID")
	}
	snap := Snapshot{Limits: c.limits()}
	if len(saved) > 0 && string(saved) != "null" {
		if err := json.Unmarshal(saved, &snap); err != nil {
			return "", nil, err
		}
	}
	if snap.Prompt == "" {
		prepared, err := readDefinition(c, t)
		if err != nil {
			return "", nil, err
		}
		c = prepared.Config
		def, instructions, repo, alias, instructionPath := prepared.Definition, prepared.Instructions, prepared.Repository, prepared.Alias, prepared.InstructionPath
		snap.Inputs = prepared.Inputs
		snap.Outputs = prepared.Outputs
		useIsolated := isolated(c.UseIsolatedWorkspace)
		snap.UseIsolatedWorkspace = &useIsolated
		snap.RepositoryPath = repo
		commitRaw, err := gitCommand(ctx, repo, "rev-parse", "HEAD^{commit}").Output()
		if err != nil && useIsolated {
			return "", nil, errors.New("repository HEAD is unavailable")
		}
		commit := strings.TrimSpace(string(commitRaw))
		digest := ""
		if useIsolated {
			// Retain the repository bytes independently of future Git branch changes/GC.
			sources := filepath.Join(c.WorkspaceRoot, "sources")
			if err = os.MkdirAll(sources, 0700); err != nil {
				return "", nil, err
			}
			tmp, err := os.CreateTemp(sources, ".archive-")
			if err != nil {
				return "", nil, err
			}
			defer os.Remove(tmp.Name())
			cmd := gitCommand(ctx, repo, "archive", "--format=tar", commit)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				tmp.Close()
				return "", nil, err
			}
			if err = cmd.Start(); err != nil {
				tmp.Close()
				return "", nil, err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(pipe, (1<<30)+1))
			if copyErr != nil || n > 1<<30 {
				cmd.Process.Kill()
			}
			waitErr := cmd.Wait()
			closeErr := tmp.Close()
			if copyErr != nil || waitErr != nil || closeErr != nil || n > 1<<30 {
				return "", nil, errors.New("repository archive unavailable or exceeds 1 GiB")
			}
			digest = hex.EncodeToString(hash.Sum(nil))
			if err = os.Rename(tmp.Name(), filepath.Join(sources, digest)); err != nil {
				return "", nil, err
			}
		}
		raw, _ := json.Marshal(def)
		definitionHash := sha256.Sum256(append(raw, instructions...))
		snap.TaskType = t.Type
		snap.Task = def
		snap.Instructions = string(instructions)
		snap.DefinitionHash = hex.EncodeToString(definitionHash[:])
		snap.InstructionPath, err = filepath.Rel(repo, instructionPath)
		if err != nil {
			return "", nil, err
		}
		snap.InstructionPath = filepath.ToSlash(snap.InstructionPath)
		snap.Repository = alias
		snap.Commit = commit
		snap.SourceHash = digest
		snap.Parameters = t.Parameters
		snap.Upstream = upstream
		snap.Prompt = assignment(string(instructions), snap.Inputs, snap.Outputs)
		if len(upstream) > 0 && string(upstream) != "null" {
			snap.Prompt += "\nAccepted predecessor artifacts are available in the evidence directory specified for this attempt; use the declared upstream input paths."
		}
		if useIsolated {
			snap.Prompt += "\nPredecessor evidence directory: .queue-inputs/ (when artifacts were supplied)."
		}
		snap.Prompt += "\n\nReport meaningful progress with queue_progress. Finish with queue_submit_result (outcome completed, blocked, or failed), a concise summary and artifact paths relative to your workspace. A chat response alone does not complete the task."
	}
	if !isolated(snap.UseIsolatedWorkspace) {
		return prepareShared(c, snap, attempt)
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(snap.SourceHash) {
		return "", nil, errors.New("invalid retained source snapshot")
	}
	source, err := os.Open(filepath.Join(c.WorkspaceRoot, "sources", snap.SourceHash))
	if err != nil {
		return "", nil, err
	}
	defer source.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, source); err != nil {
		return "", nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != snap.SourceHash {
		return "", nil, errors.New("retained source integrity check failed")
	}
	if _, err = source.Seek(0, 0); err != nil {
		return "", nil, err
	}
	workspace := filepath.Join(c.WorkspaceRoot, "attempts", attempt)
	if err = os.RemoveAll(workspace); err != nil {
		return "", nil, err
	}
	if err = os.MkdirAll(workspace, 0700); err != nil {
		return "", nil, err
	}
	if err = unpack(source, workspace); err != nil {
		return "", nil, err
	}
	if !safeRelative(snap.InstructionPath) {
		return "", nil, errors.New("invalid snapshotted instruction path")
	}
	instructionDest := filepath.Join(workspace, filepath.FromSlash(snap.InstructionPath))
	if err = os.MkdirAll(filepath.Dir(instructionDest), 0700); err != nil {
		return "", nil, err
	}
	if err = os.WriteFile(instructionDest, []byte(snap.Instructions), 0600); err != nil {
		return "", nil, err
	}
	if err = materializeInputs(c, workspace, snap.Upstream); err != nil {
		return "", nil, err
	}
	if err = prepareContract(workspace, filepath.Join(workspace, ".queue-inputs"), &snap); err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(snap)
	return workspace, raw, err
}

// Shared mode never exports, cleans or overlays the user's checkout. Only the
// attempt-owned evidence directory is rebuilt before launch.
func prepareShared(c Config, snap Snapshot, attempt string) (string, json.RawMessage, error) {
	repo, err := filepath.EvalSymlinks(c.Repositories[snap.Repository])
	if err != nil || !filepath.IsAbs(repo) || repo != snap.RepositoryPath {
		return "", nil, errors.New("saved shared repository is unavailable or no longer approved")
	}
	info, err := os.Stat(repo)
	if err != nil || !info.IsDir() {
		return "", nil, errors.New("shared repository is not a directory")
	}
	attemptRoot := filepath.Join(c.WorkspaceRoot, "attempts", attempt)
	// Keep scheduler-owned input material out of the shared checkout.
	rel, err := filepath.Rel(repo, attemptRoot)
	if err != nil || rel == "." || safeRelative(filepath.ToSlash(rel)) {
		return "", nil, errors.New("shared attempt storage must be outside the repository")
	}
	if err = os.RemoveAll(attemptRoot); err != nil {
		return "", nil, err
	}
	if err = os.MkdirAll(attemptRoot, 0700); err != nil {
		return "", nil, err
	}
	if err = materializeInputs(c, attemptRoot, snap.Upstream); err != nil {
		return "", nil, err
	}
	snap.InputsDirectory = filepath.Join(attemptRoot, ".queue-inputs")
	if err = os.MkdirAll(snap.InputsDirectory, 0700); err != nil {
		return "", nil, err
	}
	if snap.BasePrompt == "" {
		snap.BasePrompt = snap.Prompt
	}
	snap.Prompt = snap.BasePrompt + "\n\nWorkspace mode: shared filesystem. Your working directory is " + repo + ". Uncommitted files are visible and edits affect the existing checkout. Source files are not frozen for retries. Do not reset, clean or overwrite unrelated changes.\nPredecessor evidence directory for this attempt: " + snap.InputsDirectory + "\nRead accepted artifacts from that directory; do not use a shared .queue-inputs directory in the checkout. Submit artifact paths relative to the working directory."
	if err = prepareContract(repo, snap.InputsDirectory, &snap); err != nil {
		return "", nil, err
	}
	raw, err := json.Marshal(snap)
	return repo, raw, err
}

func unpack(r io.Reader, root string) error {
	tr := tar.NewReader(r)
	var total int64
	count := 0
	for {
		h, e := tr.Next()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
		// git archive adds a global PAX header carrying the commit ID, not a file.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		count++
		total += h.Size
		if count > 100000 || total > 1<<30 {
			return errors.New("repository snapshot exceeds limits")
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !safeRelative(name) {
			return errors.New("unsafe archive path")
		}
		dest := filepath.Join(root, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if e = os.MkdirAll(dest, 0700); e != nil {
				return e
			}
		case tar.TypeReg:
			if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
				return e
			}
			f, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode)&0700)
			if e != nil {
				return e
			}
			_, e = io.Copy(f, tr)
			f.Close()
			if e != nil {
				return e
			}
		default:
			return errors.New("repository snapshot contains unsupported link or special file")
		}
	}
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func archiveArtifacts(c Config, workspace string, paths []string) ([]Artifact, error) {
	if len(paths) > 50 {
		return nil, errors.New("at most 50 artifacts")
	}
	out := []Artifact{}
	seen := map[string]bool{}
	var total int64
	root := filepath.Join(c.WorkspaceRoot, "artifacts")
	if e := os.MkdirAll(root, 0700); e != nil {
		return nil, e
	}
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		if !safeRelative(p) {
			return nil, errors.New("invalid artifact path")
		}
		full := filepath.Join(workspace, p)
		resolved, e := filepath.EvalSymlinks(full)
		if e != nil {
			return nil, e
		}
		rel, e := filepath.Rel(workspace, resolved)
		if e != nil || !safeRelative(filepath.ToSlash(rel)) {
			return nil, errors.New("artifact escapes workspace")
		}
		f, e := os.Open(resolved)
		if e != nil {
			return nil, e
		}
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() {
			f.Close()
			return nil, errors.New("artifact must be a regular file")
		}
		data, e := io.ReadAll(io.LimitReader(f, 16<<20+1))
		f.Close()
		total += int64(len(data))
		if e != nil || len(data) > 16<<20 || total > 64<<20 {
			return nil, errors.New("artifact size limit exceeded")
		}
		hash := sha256.Sum256(data)
		digest := hex.EncodeToString(hash[:])
		dest := filepath.Join(root, digest)
		f, e = os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if os.IsExist(e) {
			existing, err := os.ReadFile(dest)
			if err != nil || sha256.Sum256(existing) != hash {
				return nil, errors.New("artifact integrity check failed")
			}
		} else if e != nil {
			return nil, e
		} else {
			_, e = f.Write(data)
			f.Close()
			if e != nil {
				return nil, e
			}
		}
		out = append(out, Artifact{p, digest, int64(len(data))})
	}
	return out, nil
}

// Make verified accepted evidence available to a successor without exposing a
// predecessor's mutable workspace or the shared artifact directory to its prompt.
func materializeInputs(c Config, workspace string, upstream json.RawMessage) error {
	if len(upstream) == 0 {
		return nil
	}
	var result Submission
	if err := json.Unmarshal(upstream, &result); err != nil {
		return errors.New("invalid predecessor result")
	}
	if len(result.Artifacts) == 0 {
		return nil
	}
	if len(result.Artifacts) > 50 {
		return errors.New("too many predecessor artifacts")
	}
	root := filepath.Join(workspace, ".queue-inputs")
	if err := os.Mkdir(root, 0700); err != nil {
		return errors.New("reserved .queue-inputs directory already exists or is inaccessible")
	}
	var total int64
	for _, artifact := range result.Artifacts {
		if !safeRelative(artifact.Path) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(artifact.SHA256) {
			return errors.New("invalid predecessor artifact reference")
		}
		file, err := os.Open(filepath.Join(c.WorkspaceRoot, "artifacts", artifact.SHA256))
		if err != nil {
			return errors.New("accepted predecessor artifact is missing")
		}
		data, err := io.ReadAll(io.LimitReader(file, (16<<20)+1))
		file.Close()
		total += int64(len(data))
		hash := sha256.Sum256(data)
		if err != nil || int64(len(data)) != artifact.Size || len(data) > 16<<20 || total > 64<<20 || hex.EncodeToString(hash[:]) != artifact.SHA256 {
			return errors.New("predecessor artifact integrity or size check failed")
		}
		dest := filepath.Join(root, artifact.Path)
		if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		file, err = os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if err != nil {
			return err
		}
		_, err = file.Write(data)
		file.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
