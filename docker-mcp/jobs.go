package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// JobState reflects the lifecycle of a single command invocation.
type JobState string

const (
	JobStateRunning  JobState = "running"
	JobStateExited   JobState = "exited"   // terminated normally (with some exit code)
	JobStateKilled   JobState = "killed"   // killed by signal (cancel or timeout)
	JobStateFailed   JobState = "failed"   // failed to start (e.g. exec not found)
	JobStateTimedOut JobState = "timedout" // hit the profile's TimeoutSeconds
)

// Job is one invocation of a profile. It owns a log file on disk and an
// in-memory ring of recent lines for cheap tailing.
type Job struct {
	ID          string    `json:"id"`
	Profile     string    `json:"profile"`
	Argv        []string  `json:"argv"`
	Cwd         string    `json:"cwd"`
	StartedAt   time.Time     `json:"started_at"`
	FinishedAt  time.Time     `json:"finished_at,omitempty"`
	State       JobState      `json:"state"`
	ExitCode    int           `json:"exit_code"`
	LinesLogged atomic.Int64  `json:"-"` // exposed via Snapshot()
	LogPath     string        `json:"log_path"`
	Error       string        `json:"error,omitempty"`

	// runtime — not serialized
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	logFile   *os.File
	tailMu    sync.Mutex
	tailLines []string // ring buffer of recent lines
	tailMax   int
	doneCh    chan struct{}
	mu        sync.Mutex
}

// JobManager tracks all jobs the server has started, enforces per-profile
// concurrency limits, and routes status / tail requests.
type JobManager struct {
	cfg    *Config
	logDir string

	mu      sync.Mutex
	jobs    map[string]*Job
	counter atomic.Uint64
}

func NewJobManager(cfg *Config) (*JobManager, error) {
	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		return nil, fmt.Errorf("create log dir %s: %w", cfg.LogDir, err)
	}
	return &JobManager{
		cfg:    cfg,
		logDir: cfg.LogDir,
		jobs:   make(map[string]*Job),
	}, nil
}

// Start launches a new job for the named profile. Enforces the per-profile
// concurrency limit. Returns the new job, or an error if the profile is
// unknown or the limit is hit.
func (m *JobManager) Start(profileName string) (*Job, error) {
	prof := m.cfg.FindProfile(profileName)
	if prof == nil {
		return nil, fmt.Errorf("unknown profile %q", profileName)
	}

	m.mu.Lock()
	if m.cfg.MaxConcurrentPerProfile > 0 {
		running := 0
		for _, j := range m.jobs {
			if j.Profile == profileName && j.State == JobStateRunning {
				running++
			}
		}
		if running >= m.cfg.MaxConcurrentPerProfile {
			m.mu.Unlock()
			return nil, fmt.Errorf("profile %q already has %d running job(s) (max=%d)",
				profileName, running, m.cfg.MaxConcurrentPerProfile)
		}
	}

	id := fmt.Sprintf("%d-%s", time.Now().Unix(), shortID(m.counter.Add(1)))
	logPath := filepath.Join(m.logDir, id+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("open log file: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, prof.Argv[0], prof.Argv[1:]...)
	cmd.Dir = prof.Cwd
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if m.cfg.InheritEnv {
		// Inherit full host environment, plus any profile-specific overrides
		cmd.Env = append(os.Environ(), envMapToSlice(prof.Env)...)
	} else {
		cmd.Env = buildEnv(prof.Env)
	}
	// New process group so we can kill the whole tree on cancel/timeout.
	cmd.SysProcAttr = sysProcAttrNewPGroup()

	job := &Job{
		ID:        id,
		Profile:   prof.Name,
		Argv:      prof.Argv,
		Cwd:       prof.Cwd,
		StartedAt: time.Now(),
		State:     JobStateRunning,
		LogPath:   logPath,
		cmd:       cmd,
		cancel:    cancel,
		logFile:   logFile,
		tailMax:   500, // last 500 lines kept in memory for cheap tailing
		doneCh:    make(chan struct{}),
	}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		cancel()
		job.State = JobStateFailed
		job.Error = err.Error()
		job.FinishedAt = time.Now()
		close(job.doneCh)
		m.jobs[id] = job
		m.mu.Unlock()
		return job, nil
	}

	m.jobs[id] = job
	m.mu.Unlock()

	// Tee output into the in-memory tail ring. We start a separate
	// goroutine that re-reads the log file as it grows. This avoids
	// taking ownership of the pipe and lets the kernel page-cache do
	// the heavy lifting for the on-disk log.
	go job.tailWorker()

	// Reaper goroutine.
	go func() {
		var timer *time.Timer
		if prof.TimeoutSeconds > 0 {
			timer = time.AfterFunc(time.Duration(prof.TimeoutSeconds)*time.Second, func() {
				job.mu.Lock()
				if job.State == JobStateRunning {
					job.State = JobStateTimedOut
				}
				job.mu.Unlock()
				killGroup(cmd)
			})
		}

		err := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}

		job.mu.Lock()
		job.FinishedAt = time.Now()
		if job.State == JobStateRunning {
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok {
					if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
						job.State = JobStateKilled
					} else {
						job.State = JobStateExited
					}
					job.ExitCode = exitErr.ExitCode()
				} else {
					job.State = JobStateFailed
					job.Error = err.Error()
				}
			} else {
				job.State = JobStateExited
				job.ExitCode = 0
			}
		} else {
			// Already in killed/timedout — record exit code if we have one
			if exitErr, ok := err.(*exec.ExitError); ok {
				job.ExitCode = exitErr.ExitCode()
			}
		}
		job.mu.Unlock()

		// Give the tail worker a moment to flush the final log lines,
		// then close everything. The tail worker polls every 150ms, so
		// 300ms covers two iterations + drain.
		time.Sleep(300 * time.Millisecond)
		_ = job.logFile.Close()
		close(job.doneCh)
		cancel()
	}()

	return job, nil
}

// Get returns the job with the given id, or nil.
func (m *JobManager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// List returns a snapshot of all jobs the manager has tracked, newest first.
func (m *JobManager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// Cancel sends SIGTERM (then SIGKILL after a grace period) to the job's
// process group. Idempotent; cancelling a finished job is a no-op.
func (m *JobManager) Cancel(id string) error {
	job := m.Get(id)
	if job == nil {
		return fmt.Errorf("unknown job %q", id)
	}
	job.mu.Lock()
	if job.State != JobStateRunning {
		job.mu.Unlock()
		return nil
	}
	job.State = JobStateKilled
	job.mu.Unlock()
	killGroup(job.cmd)
	return nil
}

// Snapshot returns a serializable copy of the job's metadata.
func (j *Job) Snapshot() map[string]interface{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]interface{}{
		"id":           j.ID,
		"profile":      j.Profile,
		"argv":         j.Argv,
		"cwd":          j.Cwd,
		"started_at":   j.StartedAt.UTC().Format(time.RFC3339),
		"state":        j.State,
		"exit_code":    j.ExitCode,
		"lines_logged": j.LinesLogged.Load(),
		"log_path":     j.LogPath,
	}
	if !j.FinishedAt.IsZero() {
		out["finished_at"] = j.FinishedAt.UTC().Format(time.RFC3339)
		out["duration_seconds"] = int(j.FinishedAt.Sub(j.StartedAt).Seconds())
	} else {
		out["duration_seconds"] = int(time.Since(j.StartedAt).Seconds())
	}
	if j.Error != "" {
		out["error"] = j.Error
	}
	return out
}

// Tail returns up to maxLines log lines starting at sinceLine (1-based).
// If sinceLine is 0, returns the last maxLines. Returns the next cursor
// (the line number to pass next time), whether the job has finished, and
// the current job state.
func (j *Job) Tail(sinceLine, maxLines int) (lines []string, nextCursor int, done bool, state JobState) {
	if maxLines <= 0 {
		maxLines = 200
	}
	if maxLines > 2000 {
		maxLines = 2000
	}

	total := int(j.LinesLogged.Load())
	j.tailMu.Lock()
	tailBuf := append([]string(nil), j.tailLines...)
	j.tailMu.Unlock()

	j.mu.Lock()
	state = j.State
	j.mu.Unlock()
	done = state != JobStateRunning

	if total == 0 {
		return nil, 1, done, state
	}

	// Determine the absolute line range we want.
	startAbs := sinceLine
	if startAbs <= 0 {
		startAbs = total - maxLines + 1
		if startAbs < 1 {
			startAbs = 1
		}
	}
	endAbs := startAbs + maxLines - 1
	if endAbs > total {
		endAbs = total
	}
	if startAbs > total {
		return nil, total + 1, done, state
	}

	// The in-memory buffer holds the most recent `tailMax` lines, i.e.
	// absolute lines [total - len(tailBuf) + 1 .. total]. If the requested
	// range falls inside that window we serve from memory; otherwise we
	// re-read the on-disk log (cold-path).
	bufStart := total - len(tailBuf) + 1
	if startAbs >= bufStart {
		offset := startAbs - bufStart
		count := endAbs - startAbs + 1
		if offset+count > len(tailBuf) {
			count = len(tailBuf) - offset
		}
		lines = make([]string, count)
		copy(lines, tailBuf[offset:offset+count])
	} else {
		lines = readLinesRange(j.LogPath, startAbs, endAbs)
	}

	return lines, endAbs + 1, done, state
}

// Wait blocks until the job exits or ctx is cancelled. Useful for tests.
func (j *Job) Wait(ctx context.Context) error {
	select {
	case <-j.doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func shortID(n uint64) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789" // no l/i/o/0/1
	out := make([]byte, 6)
	for i := range out {
		out[i] = alphabet[n%uint64(len(alphabet))]
		n /= uint64(len(alphabet))
	}
	return string(out)
}

// envMapToSlice converts a map of env vars to a slice of "K=V" strings.
func envMapToSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// buildEnv returns a minimal environment for spawned commands. We do NOT
// inherit the host environment by default — the host may have credentials
// or paths we don't want exposed to the child. PATH is passed through so
// `docker`, `compose`, etc. resolve normally.
func buildEnv(extra map[string]string) []string {
	env := []string{}
	if path := os.Getenv("PATH"); path != "" {
		env = append(env, "PATH="+path)
	}
	if home := os.Getenv("HOME"); home != "" {
		env = append(env, "HOME="+home)
	}
	// Docker Desktop on macOS / Linux uses these to find the daemon socket
	// and CLI plugins. Pass them through so the child can talk to docker.
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_BUILDKIT", "BUILDX_CONFIG", "USER", "LOGNAME"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}
