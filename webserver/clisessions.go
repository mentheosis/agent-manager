package webserver

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cliSessionJSON represents a Claude Code CLI session discovered on disk.
type cliSessionJSON struct {
	SessionID string `json:"session_id"`
	Project   string `json:"project"`   // decoded project path
	CWD       string `json:"cwd"`       // working directory from first message
	Model     string `json:"model"`     // model used (from first assistant message)
	GitBranch string `json:"git_branch"`
	Version   string `json:"version"`  // Claude Code version
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	FilePath  string `json:"file_path"` // path to the JSONL file
	SizeBytes int64  `json:"size_bytes"`
}

// discoverCLISessions scans ~/.claude/projects/ for CLI session JSONL files
// and extracts metadata from each.
func discoverCLISessions() ([]cliSessionJSON, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(homeDir, ".claude", "projects")

	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var sessions []cliSessionJSON

	for _, projEntry := range entries {
		if !projEntry.IsDir() {
			continue
		}
		projPath := filepath.Join(projectsDir, projEntry.Name())
		projectName := decodeProjectPath(projEntry.Name())

		files, err := os.ReadDir(projPath)
		if err != nil {
			continue
		}

		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			// Skip non-UUID filenames (like memory files)
			name := strings.TrimSuffix(f.Name(), ".jsonl")
			if len(name) != 36 || strings.Count(name, "-") != 4 {
				continue
			}

			filePath := filepath.Join(projPath, f.Name())
			info, err := f.Info()
			if err != nil {
				continue
			}

			sess := cliSessionJSON{
				SessionID: name,
				Project:   projectName,
				FilePath:  filePath,
				SizeBytes: info.Size(),
				UpdatedAt: info.ModTime().Format(time.RFC3339),
			}

			// Extract metadata from the file (read first few lines + last line)
			extractCLISessionMeta(filePath, &sess)

			sessions = append(sessions, sess)
		}
	}

	// Sort by updated_at descending
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt > sessions[j].UpdatedAt
	})

	return sessions, nil
}

// decodeProjectPath converts a dash-encoded directory name back to a path.
// e.g. "-Users-kris-w-wrk-claude-squad" → "/Users/kris-w/wrk/claude-squad"
func decodeProjectPath(encoded string) string {
	if !strings.HasPrefix(encoded, "-") {
		return encoded
	}
	return "/" + strings.ReplaceAll(encoded[1:], "-", "/")
}

// jsonlEntry is a minimal struct for reading JSONL session entries.
type jsonlEntry struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	CWD       string    `json:"cwd"`
	Version   string    `json:"version"`
	GitBranch string    `json:"gitBranch"`
	Timestamp time.Time `json:"timestamp"`
	Message   *struct {
		Model string `json:"model"`
	} `json:"message"`
}

// extractCLISessionMeta reads the JSONL file to extract session metadata.
// It reads the first few lines for session info and tracks the last timestamp.
func extractCLISessionMeta(filePath string, sess *cliSessionJSON) {
	f, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// Increase buffer for large JSONL lines
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	gotMeta := false
	gotModel := false
	var lastTimestamp time.Time
	linesRead := 0

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry jsonlEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}

		// Track timestamps for created_at / updated_at
		if !entry.Timestamp.IsZero() {
			if sess.CreatedAt == "" {
				sess.CreatedAt = entry.Timestamp.Format(time.RFC3339)
			}
			lastTimestamp = entry.Timestamp
		}

		// Extract metadata from first user/assistant message
		if !gotMeta && (entry.Type == "user" || entry.Type == "assistant") {
			if entry.SessionID != "" {
				sess.SessionID = entry.SessionID
			}
			if entry.CWD != "" {
				sess.CWD = entry.CWD
			}
			if entry.Version != "" {
				sess.Version = entry.Version
			}
			if entry.GitBranch != "" {
				sess.GitBranch = entry.GitBranch
			}
			gotMeta = true
		}

		// Extract model from first assistant message
		if !gotModel && entry.Type == "assistant" && entry.Message != nil && entry.Message.Model != "" {
			sess.Model = entry.Message.Model
			gotModel = true
		}

		linesRead++
		// For large files, stop reading after getting metadata + reading enough
		// to establish timestamps. We'll use file mod time for updated_at.
		if gotMeta && gotModel && linesRead > 20 {
			break
		}
	}

	if !lastTimestamp.IsZero() && linesRead <= 20 {
		// If we read the whole file (small file), use last timestamp
		sess.UpdatedAt = lastTimestamp.Format(time.RFC3339)
	}
	// Otherwise keep file mod time as updated_at (set by caller)
}
