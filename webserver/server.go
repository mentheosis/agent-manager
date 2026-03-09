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
	storage   *session.Storage
	program   string
	autoYes   bool
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

	return &Server{
		instances: instances,
		storage:   storage,
		program:   program,
		autoYes:   autoYes,
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

// pollMetadata updates status for all instances (like the TUI's tickUpdateMetadataCmd)
func (s *Server) pollMetadata() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for _, inst := range s.instances {
			if !inst.Started() || inst.Paused() {
				continue
			}
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
		s.mu.Unlock()
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
		content, err := inst.PreviewFullHistory()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"content": content})

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
		s.handleWebSocket(w, r, inst)
		s.mu.Lock() // Re-acquire for deferred unlock
		return

	default:
		// GET /api/instances/{title} - return instance details
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.toJSON(inst))
	}
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request, inst *session.Instance) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.ErrorLog.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Stream terminal output
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	var lastContent string
	for range ticker.C {
		s.mu.RLock()
		content, err := inst.Preview()
		status := statusString(inst.Status)
		s.mu.RUnlock()

		if err != nil {
			break
		}

		if content != lastContent {
			lastContent = content
			msg := map[string]string{
				"type":    "output",
				"content": content,
				"status":  status,
			}
			if err := conn.WriteJSON(msg); err != nil {
				break
			}
		}
	}
}

func Run(program string, autoYes bool, port int) error {
	srv, err := NewServer(program, autoYes)
	if err != nil {
		return err
	}

	go srv.pollMetadata()

	mux := http.NewServeMux()

	// API routes
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

	// Static files
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "webserver/static/index.html")
	})

	addr := fmt.Sprintf(":%d", port)
	fmt.Printf("Claude Squad Web UI running at http://localhost%s\n", addr)
	return http.ListenAndServe(addr, mux)
}
