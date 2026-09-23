package taskqueues

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type preparedDefinition struct {
	Config          Config
	Definition      Definition
	Instructions    []byte
	Repository      string
	Alias           string
	InstructionPath string
	Inputs          []string
	Outputs         []string
}

func readDefinition(c Config, t Task) (*preparedDefinition, error) {
	if c.ProfilePath != "" {
		raw, err := os.ReadFile(c.ProfilePath)
		if err != nil {
			return nil, errors.New("profile file unavailable")
		}
		var profiles map[string]struct {
			BasePath             string                `json:"base_path"`
			Tasks                map[string]Definition `json:"tasks"`
			UseIsolatedWorkspace *bool                 `json:"use_isolated_workspace"`
			Repositories         map[string]string     `json:"repositories"`
		}
		if err = json.Unmarshal(raw, &profiles); err != nil {
			return nil, errors.New("invalid profile file")
		}
		profile, ok := profiles[c.QueueID]
		if !ok {
			return nil, errors.New("queue profile no longer exists")
		}
		c.UseIsolatedWorkspace = profile.UseIsolatedWorkspace
		c.BasePath = profile.BasePath
		c.Tasks = profile.Tasks
		c.Repositories = profile.Repositories
	}
	def, ok := c.Tasks[t.Type]
	if !ok {
		return nil, errors.New("unknown task type in queue profile")
	}
	if def.Provider == "" {
		def.Provider = "codex"
	}
	if def.Permission == "" {
		if def.Provider == "claude" {
			def.Permission = "acceptEdits"
		} else {
			def.Permission = "workspace-write"
		}
	}
	if def.Permission == "dangerFullAccess" {
		def.Permission = "danger-full-access"
	}
	if def.Permission == "bypassPermission" {
		def.Permission = "bypassPermissions"
	}
	valid := map[string]map[string]bool{"codex": {"read-only": true, "workspace-write": true, "danger-full-access": true}, "claude": {"default": true, "acceptEdits": true, "plan": true, "bypassPermissions": true}}
	if !valid[def.Provider][def.Permission] {
		return nil, errors.New("invalid task provider or permission")
	}
	schemaRaw, err := compileParameterMap(def.Parameters)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.LoadURL = func(string) (io.ReadCloser, error) { return nil, errors.New("external schema references are disabled") }
	if err = compiler.AddResource("schema.json", bytes.NewReader(schemaRaw)); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile("schema.json")
	if err != nil {
		return nil, err
	}
	var params any
	decoder := json.NewDecoder(bytes.NewReader(t.Parameters))
	decoder.UseNumber()
	if err = decoder.Decode(&params); err != nil {
		return nil, err
	}
	if err = schema.Validate(params); err != nil {
		return nil, fmt.Errorf("invalid task parameters: %w", err)
	}
	if c.BasePath != "" && !safeRelative(c.BasePath) {
		return nil, errors.New("base_path must be a repository-relative path without traversal")
	}
	if !filepath.IsAbs(def.Instructions) {
		if len(c.Repositories) != 1 {
			return nil, errors.New("relative instructions require one repository")
		}
		if !safeRelative(def.Instructions) {
			return nil, errors.New("invalid instruction path")
		}
		for _, root := range c.Repositories {
			def.Instructions = filepath.Join(root, c.BasePath, def.Instructions)
		}
	}
	instructionPath, err := filepath.EvalSymlinks(def.Instructions)
	if err != nil || !filepath.IsAbs(def.Instructions) {
		return nil, errors.New("instructions must name an accessible absolute file")
	}
	repo, alias := "", ""
	for name, candidate := range c.Repositories {
		real, err := filepath.EvalSymlinks(candidate)
		if err != nil || !filepath.IsAbs(real) {
			continue
		}
		rel, err := filepath.Rel(real, instructionPath)
		if err == nil && safeRelative(filepath.ToSlash(rel)) && len(real) > len(repo) {
			repo, alias = real, name
		}
	}
	if repo == "" {
		return nil, errors.New("instructions must be inside an approved repository")
	}
	if def.Reviewer != nil {
		r := *def.Reviewer
		if r.Provider == "" {
			r.Provider = "codex"
		}
		if r.Permission == "" {
			r.Permission = "read-only"
			if r.Provider == "claude" {
				r.Permission = "plan"
			}
		}
		if !((r.Provider == "codex" && r.Permission == "read-only") || (r.Provider == "claude" && r.Permission == "plan")) {
			return nil, errors.New("reviewer requires codex read-only or claude plan permissions")
		}

		name := r.Instructions
		if !filepath.IsAbs(name) {
			if !safeRelative(name) {
				return nil, errors.New("invalid reviewer instruction path")
			}
			name = filepath.Join(repo, c.BasePath, name)
		}
		real, e := filepath.EvalSymlinks(name)
		if e != nil {
			return nil, e
		}
		rel, e := filepath.Rel(repo, real)
		if e != nil || !safeRelative(filepath.ToSlash(rel)) {
			return nil, errors.New("reviewer instructions must be inside the task repository")
		}
		info, e := os.Stat(real)
		if e != nil || !info.Mode().IsRegular() || info.Size() > 2<<20 {
			return nil, errors.New("invalid reviewer instruction file")
		}
		content, e := os.ReadFile(real)
		if e != nil {
			return nil, e
		}
		r.Content = string(content)
		def.Reviewer = &r
	}
	file, err := os.Open(instructionPath)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("instructions must be a regular file")
	}
	instructions, err := io.ReadAll(io.LimitReader(file, (2<<20)+1))
	file.Close()
	if err != nil || len(instructions) > 2<<20 {
		return nil, errors.New("instructions exceed size limit")
	}

	rendered, err := renderText(string(instructions), params)
	if err != nil {
		return nil, err
	}
	inputs, err := renderPaths(def.Inputs, params, true)
	if err != nil {
		return nil, err
	}
	outputs, err := renderPaths(def.Outputs, params, false)
	if err != nil {
		return nil, err
	}
	for _, paths := range [][]string{inputs, outputs} {
		for i, path := range paths {
			prefix := ""
			if strings.HasPrefix(path, "upstream:") {
				prefix = "upstream:"
				path = strings.TrimPrefix(path, prefix)
			}
			paths[i] = prefix + filepath.ToSlash(filepath.Join(c.BasePath, path))
		}
	}
	return &preparedDefinition{c, def, []byte(rendered), repo, alias, instructionPath, inputs, outputs}, nil
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// Literal substitution only: values are never evaluated as code or templates.
func renderText(template string, params any) (string, error) {
	values, ok := params.(map[string]any)
	if !ok {
		return "", errors.New("parameters must be an object")
	}
	var problem error
	rest := placeholder.ReplaceAllString(template, "")
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return "", errors.New("invalid template expression; only {{parameter}} is supported")
	}
	result := placeholder.ReplaceAllStringFunc(template, func(match string) string {
		name := placeholder.FindStringSubmatch(match)[1]
		value, exists := values[name]
		if !exists {
			problem = fmt.Errorf("missing template parameter %q", name)
			return ""
		}
		if text, ok := value.(string); ok {
			return text
		}
		raw, err := json.Marshal(value)
		if err != nil {
			problem = err
		}
		return string(raw)
	})
	return result, problem
}
func renderPaths(templates []string, params any, inputs bool) ([]string, error) {
	if len(templates) > 50 {
		return nil, errors.New("at most 50 declared input/output files")
	}
	result := []string{}
	seen := map[string]bool{}
	for _, template := range templates {
		rendered, err := renderText(template, params)
		if err != nil {
			return nil, err
		}
		path := rendered
		if inputs {
			path = strings.TrimPrefix(path, "upstream:")
		}
		if !safeRelative(path) || strings.ContainsAny(path, "\x00\r\n") || strings.Contains(path, ":") {
			return nil, fmt.Errorf("invalid file path %q", rendered)
		}
		if seen[rendered] {
			return nil, fmt.Errorf("duplicate file path %q", rendered)
		}
		seen[rendered] = true
		result = append(result, rendered)
	}
	return result, nil
}
func assignment(instructions string, inputs, outputs []string) string {
	prompt := instructions
	local, upstream := []string{}, []string{}
	for _, path := range inputs {
		if strings.HasPrefix(path, "upstream:") {
			upstream = append(upstream, strings.TrimPrefix(path, "upstream:"))
		} else {
			local = append(local, path)
		}
	}
	if len(local) > 0 {
		prompt += "\n\nRequired repository inputs (relative to the working directory; these are authoritative task inputs and do not require predecessor approval):\n- " + strings.Join(local, "\n- ")
	}
	if len(upstream) > 0 {
		prompt += "\n\nRequired accepted predecessor inputs (relative to the supplied evidence directory):\n- " + strings.Join(upstream, "\n- ")
	} else {
		prompt += "\n\nThis task has no required predecessor inputs. An absent or empty predecessor directory is expected and is not a blocker."
	}
	if len(outputs) > 0 {
		prompt += "\n\nRequired output files (relative to the working directory; create or rewrite each during this attempt):"
		for _, p := range outputs {
			prompt += "\n- " + p
		}
	}
	return prompt
}

// File signatures detect stale pre-existing shared outputs. They establish file
// production/retention, not semantic correctness of the contents.
func fileSignature(root, path string) (string, error) {
	full := filepath.Join(root, path)
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	absoluteRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absoluteRoot, resolved)
	if err != nil || !safeRelative(filepath.ToSlash(rel)) {
		return "", errors.New("file escapes allowed directory")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("expected regular file")
	}
	if info.Size() > 16<<20 {
		return "", errors.New("file exceeds 16 MiB")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (16<<20)+1))
	if err != nil || n > 16<<20 {
		return "", errors.New("file exceeds size/read limit")
	}
	return fmt.Sprintf("%d:%d:%s", info.ModTime().UnixNano(), n, hex.EncodeToString(hash.Sum(nil))), nil
}
func prepareContract(workspace, evidence string, snap *Snapshot) error {
	snap.BasePrompt = assignment(snap.Instructions, snap.Inputs, snap.Outputs)
	snap.Prompt = snap.BasePrompt + "\n\nReport meaningful progress with queue_progress. Finish with queue_submit_result (completed, blocked, or failed) and artifact paths relative to the working directory."
	snap.Prompt += "\nWorking directory: " + workspace
	if !isolated(snap.UseIsolatedWorkspace) {
		snap.Prompt += "\nWorkspace mode: shared filesystem. Preserve unrelated local edits; do not reset or clean the checkout."
	}
	snap.Prompt += "\n\nResolved required input locations:"
	for _, path := range snap.Inputs {
		root := workspace
		if strings.HasPrefix(path, "upstream:") {
			root = evidence
			path = strings.TrimPrefix(path, "upstream:")
		}
		snap.Prompt += "\n- " + filepath.Join(root, path)
	}
	for _, path := range snap.Inputs {
		if strings.HasPrefix(path, "upstream:") {
			snap.Prompt += "\nPredecessor evidence directory: " + evidence
			break
		}
	}

	snap.InputSignatures = map[string]string{}
	for _, input := range snap.Inputs {
		root, path := workspace, input
		if strings.HasPrefix(input, "upstream:") {
			root = evidence
			path = strings.TrimPrefix(input, "upstream:")
		}
		signature, err := fileSignature(root, path)
		if err != nil {
			return fmt.Errorf("required input %q: %w", input, err)
		}
		snap.InputSignatures[input] = signature
	}
	snap.OutputBaseline = map[string]string{}
	for _, path := range snap.Outputs {
		signature, err := fileSignature(workspace, path)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("output path %q: %w", path, err)
		}
		snap.OutputBaseline[path] = signature
	}
	return nil
}
func requiredOutputs(workspace string, snap Snapshot) error {
	for _, path := range snap.Outputs {
		signature, err := fileSignature(workspace, path)
		if err != nil {
			return fmt.Errorf("required output %q: %w", path, err)
		}
		if snap.OutputBaseline[path] == signature {
			return fmt.Errorf("required output %q was not created or rewritten during this attempt", path)
		}
	}
	return nil
}
