package webserver

import (
	"claude-squad/config"
	"claude-squad/log"
	"claude-squad/session"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Server struct {
	mu        sync.RWMutex
	instances []*session.Instance
	convLogs  map[string]*ConversationLog
	storage   *session.Storage
	program   string
	autoYes   bool

	// Status broadcast: pollMetadata writes, status WS clients read
	statusMu      sync.Mutex
	lastStatuses  map[string]string        // title -> last known status string
	statusClients map[*websocket.Conn]bool // connected status WS clients
}

func NewServer(program string, autoYes bool) (*Server, error) {
	appState := config.LoadState()
	storage, err := session.NewStorage(appState)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}

	instances, err := storage.LoadInstances()
	if err != nil {
		return nil, fmt.Errorf("failed to load instances: %w", err)
	}

	convLogs := make(map[string]*ConversationLog)
	for _, inst := range instances {
		convLogs[inst.Title] = NewConversationLog()
	}

	return &Server{
		instances:     instances,
		convLogs:      convLogs,
		storage:       storage,
		program:       program,
		autoYes:       autoYes,
		lastStatuses:  make(map[string]string),
		statusClients: make(map[*websocket.Conn]bool),
	}, nil
}

type instanceJSON struct {
	Title       string `json:"title"`
	Status      string `json:"status"`
	Branch      string `json:"branch"`
	Program     string `json:"program"`
	Path        string `json:"path"`
	WorkDir     string `json:"work_dir"`
	GitMode     bool   `json:"git_mode"`
	CreatedAt   string `json:"created_at"`
	DiffAdded   int    `json:"diff_added"`
	DiffRemoved int    `json:"diff_removed"`
}

func statusString(s session.Status) string {
	switch s {
	case session.Running:
		return "running"
	case session.Ready:
		return "ready"
	case session.Loading:
		return "loading"
	case session.Paused:
		return "paused"
	default:
		return "unknown"
	}
}

func (s *Server) toJSON(inst *session.Instance) instanceJSON {
	j := instanceJSON{
		Title:     inst.Title,
		Status:    statusString(inst.Status),
		Branch:    inst.Branch,
		Program:   inst.Program,
		Path:      inst.Path,
		WorkDir:   inst.GetWorktreePath(),
		GitMode:   inst.GitMode,
		CreatedAt: inst.CreatedAt.Format(time.RFC3339),
	}
	// For non-git mode, work_dir is just the path
	if j.WorkDir == "" {
		j.WorkDir = inst.Path
	}
	if ds := inst.GetDiffStats(); ds != nil {
		j.DiffAdded = ds.Added
		j.DiffRemoved = ds.Removed
	}
	return j
}

func (s *Server) findInstance(title string) *session.Instance {
	for _, inst := range s.instances {
		if inst.Title == title {
			return inst
		}
	}
	return nil
}

func (s *Server) save() error {
	return s.storage.SaveInstances(s.instances)
}

// getOrCreateConvLog returns the ConversationLog for the given title, creating one if needed.
// Must be called with s.mu held.
func (s *Server) getOrCreateConvLog(title string) *ConversationLog {
	cl, ok := s.convLogs[title]
	if !ok {
		cl = NewConversationLog()
		s.convLogs[title] = cl
	}
	return cl
}

// pollMetadata updates status for all instances (like the TUI's tickUpdateMetadataCmd)
// and broadcasts status changes to all connected status WebSocket clients.
func (s *Server) pollMetadata() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		changed := make(map[string]string) // title -> new status
		for _, inst := range s.instances {
			if inst.Started() && !inst.Paused() {
				inst.CheckAndHandleTrustPrompt()
				updated, prompt := inst.HasUpdated()
				if updated {
					inst.SetStatus(session.Running)
				} else {
					if prompt {
						inst.TapEnter()
					} else {
						inst.SetStatus(session.Ready)
					}
				}
				_ = inst.UpdateDiffStats()
			}

			// Always check for status changes (including paused/loading instances)
			status := statusString(inst.Status)
			s.statusMu.Lock()
			if s.lastStatuses[inst.Title] != status {
				s.lastStatuses[inst.Title] = status
				changed[inst.Title] = status
			}
			s.statusMu.Unlock()
		}
		s.mu.Unlock()

		// Broadcast any changes
		if len(changed) > 0 {
			s.broadcastStatuses(changed)
		}
	}
}

// broadcastStatuses sends status updates to all connected status WS clients.
func (s *Server) broadcastStatuses(changed map[string]string) {
	msg := map[string]interface{}{
		"type":     "status_update",
		"statuses": changed,
	}

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	for conn := range s.statusClients {
		if err := conn.WriteJSON(msg); err != nil {
			conn.Close()
			delete(s.statusClients, conn)
		}
	}
}

// handleStatusWebSocket serves a WebSocket that pushes status changes for all instances.
func (s *Server) handleStatusWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.ErrorLog.Printf("status websocket upgrade failed: %v", err)
		return
	}

	// Collect current statuses, register client, and send initial state all
	// under statusMu to prevent concurrent writes and missed updates.
	s.mu.RLock()
	current := make(map[string]string)
	for _, inst := range s.instances {
		current[inst.Title] = statusString(inst.Status)
	}
	s.mu.RUnlock()

	s.statusMu.Lock()
	s.statusClients[conn] = true
	if len(current) > 0 {
		if err := conn.WriteJSON(map[string]interface{}{
			"type":     "status_update",
			"statuses": current,
		}); err != nil {
			delete(s.statusClients, conn)
			s.statusMu.Unlock()
			conn.Close()
			return
		}
	}
	s.statusMu.Unlock()

	// Read (and discard) messages to detect close
	defer func() {
		s.statusMu.Lock()
		delete(s.statusClients, conn)
		s.statusMu.Unlock()
		conn.Close()
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// pollOutput captures tmux output for all active instances and feeds it to their ConversationLogs.
func (s *Server) pollOutput() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.RLock()
		for _, inst := range s.instances {
			if !inst.Started() || inst.Paused() {
				continue
			}
			cl := s.convLogs[inst.Title]
			if cl == nil {
				continue
			}
			// Capture visible pane first, then full history.
			// This ordering means if a line scrolls between captures,
			// it's in full but not pane — correctly classified as scrollback.
			paneContent, err := inst.Preview()
			if err != nil {
				continue
			}
			fullContent, err := inst.PreviewFullHistory()
			if err != nil {
				continue
			}
			cl.Ingest(fullContent, paneContent)
		}
		s.mu.RUnlock()
	}
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]instanceJSON, len(s.instances))
	for i, inst := range s.instances {
		result[i] = s.toJSON(inst)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    string `json:"title"`
		Prompt   string `json:"prompt"`
		Path     string `json:"path"`
		GitMode  bool   `json:"git_mode"`
		RepoPath string `json:"repo_path"`
		Branch   string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Title == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}
	if body.Path == "" {
		body.Path = "."
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.findInstance(body.Title) != nil {
		http.Error(w, "instance with that title already exists", http.StatusConflict)
		return
	}

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:    body.Title,
		Path:     body.Path,
		RepoPath: body.RepoPath,
		Program:  s.program,
		AutoYes:  s.autoYes,
		GitMode:  body.GitMode,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if err := inst.Start(true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if body.Prompt != "" {
		// Small delay to let the session initialize
		go func() {
			time.Sleep(2 * time.Second)
			s.mu.Lock()
			defer s.mu.Unlock()
			if err := inst.SendPrompt(body.Prompt); err != nil {
				log.ErrorLog.Printf("failed to send initial prompt: %v", err)
			}
		}()
	}

	s.instances = append(s.instances, inst)
	s.convLogs[body.Title] = NewConversationLog()
	if err := s.save(); err != nil {
		log.ErrorLog.Printf("failed to save: %v", err)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(s.toJSON(inst))
}

func (s *Server) handleInstanceAction(w http.ResponseWriter, r *http.Request) {
	// Parse path: /api/instances/{title}/{action}
	path := strings.TrimPrefix(r.URL.Path, "/api/instances/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		http.Error(w, "missing instance title", http.StatusBadRequest)
		return
	}
	title := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.findInstance(title)
	if inst == nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	switch action {
	case "send":
		var body struct {
			Text string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := inst.SendPrompt(body.Text); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Record in input history
		if cl := s.convLogs[title]; cl != nil {
			cl.AddInput(body.Text)
		}
		w.WriteHeader(http.StatusOK)

	case "pause":
		if err := inst.Pause(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = s.save()
		w.WriteHeader(http.StatusOK)

	case "resume":
		if err := inst.Resume(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Ensure convLog exists for resumed instance
		s.getOrCreateConvLog(title)
		_ = s.save()
		w.WriteHeader(http.StatusOK)

	case "kill":
		if err := inst.Kill(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Remove from list
		for i, existing := range s.instances {
			if existing.Title == title {
				s.instances = append(s.instances[:i], s.instances[i+1:]...)
				break
			}
		}
		delete(s.convLogs, title)
		s.statusMu.Lock()
		delete(s.lastStatuses, title)
		s.statusMu.Unlock()
		_ = s.storage.DeleteInstance(title)
		w.WriteHeader(http.StatusOK)

	case "preview":
		content, err := inst.Preview()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"content": content})

	case "history":
		cl := s.getOrCreateConvLog(title)
		stableLines, stableSeqNo, pane := cl.GetState()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stable_lines":  stableLines,
			"stable_seq_no": stableSeqNo,
			"stable_count":  len(stableLines),
			"pane":          pane,
		})

	case "input-history":
		cl := s.getOrCreateConvLog(title)
		history := cl.GetInputHistory()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"history": history,
		})

	case "diff":
		ds := inst.GetDiffStats()
		if ds == nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"added": 0, "removed": 0, "content": ""})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"added":   ds.Added,
			"removed": ds.Removed,
			"content": ds.Content,
		})

	case "rules":
		workDir := inst.GetWorktreePath()
		if workDir == "" {
			workDir = inst.Path
		}
		rulesPath := filepath.Join(workDir, "CLAUDE.md")

		if r.Method == http.MethodGet {
			content, err := os.ReadFile(rulesPath)
			if err != nil {
				if os.IsNotExist(err) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{"content": "", "exists": false, "path": rulesPath})
					return
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"content": string(content), "exists": true, "path": rulesPath})
		} else if r.Method == http.MethodPut {
			var body struct {
				Content string `json:"content"`
			}
			bodyBytes, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := os.WriteFile(rulesPath, []byte(body.Content), 0644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "path": rulesPath})
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "ws":
		s.mu.Unlock() // Release lock for long-lived WebSocket
		s.handleWebSocket(w, r, inst, title)
		s.mu.Lock() // Re-acquire for deferred unlock
		return

	default:
		// GET /api/instances/{title} - return instance details
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.toJSON(inst))
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request, inst *session.Instance, title string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.ErrorLog.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Start a goroutine to read (and discard) client messages to detect close
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	s.mu.RLock()
	cl := s.convLogs[title]
	s.mu.RUnlock()
	if cl == nil {
		return
	}

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastStableCount int
	var lastPaneContent string

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		stableLines, _, pane := cl.GetState()

		s.mu.RLock()
		status := statusString(inst.Status)
		s.mu.RUnlock()

		// Send new stable lines as history_append
		if len(stableLines) > lastStableCount {
			newLines := stableLines[lastStableCount:]
			msg := map[string]interface{}{
				"type":  "history_append",
				"lines": newLines,
			}
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
			lastStableCount = len(stableLines)
		}

		// Send current pane (volatile) - only if changed
		paneContent := strings.Join(pane, "\n")
		if paneContent != lastPaneContent {
			lastPaneContent = paneContent
			msg := map[string]interface{}{
				"type":    "pane",
				"content": paneContent,
				"status":  status,
			}
			if err := conn.WriteJSON(msg); err != nil {
				return
			}
		}
	}
}

// sessionJSON is a lightweight representation of a saved session (read directly from state.json)
type sessionJSON struct {
	Title       string `json:"title"`
	Status      string `json:"status"`
	Branch      string `json:"branch"`
	Program     string `json:"program"`
	Path        string `json:"path"`
	GitMode     bool   `json:"git_mode"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	DiffAdded   int    `json:"diff_added"`
	DiffRemoved int    `json:"diff_removed"`
	WorktreeDir string `json:"worktree_dir"`
	RepoPath    string `json:"repo_path"`
	BranchName  string `json:"branch_name"`
	Active      bool   `json:"active"` // true if this session is loaded in the current web server
}

// handleListSessions reads state.json fresh to list ALL saved sessions, including
// those created from the terminal TUI that may not be loaded in this web server.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	appState := config.LoadState()
	rawJSON := appState.GetInstances()

	var sessionsData []session.InstanceData
	if err := json.Unmarshal(rawJSON, &sessionsData); err != nil {
		http.Error(w, "failed to read sessions: "+err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.RLock()
	activeSet := make(map[string]bool)
	for _, inst := range s.instances {
		activeSet[inst.Title] = true
	}
	s.mu.RUnlock()

	result := make([]sessionJSON, len(sessionsData))
	for i, data := range sessionsData {
		result[i] = sessionJSON{
			Title:       data.Title,
			Status:      statusString(data.Status),
			Branch:      data.Branch,
			Program:     data.Program,
			Path:        data.Path,
			GitMode:     data.GitMode,
			CreatedAt:   data.CreatedAt.Format(time.RFC3339),
			UpdatedAt:   data.UpdatedAt.Format(time.RFC3339),
			DiffAdded:   data.DiffStats.Added,
			DiffRemoved: data.DiffStats.Removed,
			WorktreeDir: data.Worktree.WorktreePath,
			RepoPath:    data.Worktree.RepoPath,
			BranchName:  data.Worktree.BranchName,
			Active:      activeSet[data.Title],
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleSessionAction handles DELETE /api/sessions/{title} - removes a session from state.json
func (s *Server) handleSessionAction(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
	title := strings.TrimSuffix(path, "/")

	if title == "" {
		http.Error(w, "missing session title", http.StatusBadRequest)
		return
	}

	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// If the session is loaded in our server, kill it first
	inst := s.findInstance(title)
	if inst != nil {
		_ = inst.Kill()
		for i, existing := range s.instances {
			if existing.Title == title {
				s.instances = append(s.instances[:i], s.instances[i+1:]...)
				break
			}
		}
	}
	delete(s.convLogs, title)
	s.statusMu.Lock()
	delete(s.lastStatuses, title)
	s.statusMu.Unlock()

	// Delete from persistent storage
	if err := s.storage.DeleteInstance(title); err != nil {
		http.Error(w, "failed to delete session: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func Run(program string, autoYes bool, port int) error {
	srv, err := NewServer(program, autoYes)
	if err != nil {
		return err
	}

	go srv.pollMetadata()
	go srv.pollOutput()

	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			srv.handleListSessions(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleSessionAction(w, r)
	})
	mux.HandleFunc("/api/instances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			srv.handleListInstances(w, r)
		case http.MethodPost:
			srv.handleCreateInstance(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/instances/", func(w http.ResponseWriter, r *http.Request) {
		srv.handleInstanceAction(w, r)
	})
	mux.HandleFunc("/api/statuses/ws", func(w http.ResponseWriter, r *http.Request) {
		srv.handleStatusWebSocket(w, r)
	})
	mux.HandleFunc("/api/cli-sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sessions, err := discoverCLISessions()
		if err != nil {
			http.Error(w, "failed to discover CLI sessions: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(sessions)
	})

	// Static files
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("webserver/static"))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "webserver/static/index.html")
	})

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Claude Squad Web UI running at http://localhost%s\n", addr)
	return http.ListenAndServe(addr, mux)
}
