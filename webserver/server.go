package webserver

import (
	"claude-squad/config"
	"claude-squad/log"
	"claude-squad/orchestrator"
	"claude-squad/session"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

	// Tracks tmux scrollback size per instance to enable delta-only capture.
	lastHistorySize map[string]int

	// Status broadcast: pollMetadata writes, status WS clients read
	statusMu      sync.Mutex
	lastAgentMeta map[string]*AgentMeta     // title -> last broadcast metadata
	statusClients map[*websocket.Conn]bool  // connected status WS clients

	// port is the port the web server is running on (set during Run)
	port int

	// nextMCPPort is the next port to assign to an orchestrator MCP server.
	nextMCPPort int
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
		instances:       instances,
		convLogs:        convLogs,
		storage:         storage,
		program:         program,
		autoYes:         autoYes,
		lastHistorySize: make(map[string]int),
		lastAgentMeta:   make(map[string]*AgentMeta),
		statusClients:   make(map[*websocket.Conn]bool),
	}, nil
}

type instanceJSON struct {
	Title        string   `json:"title"`
	DisplayTitle string   `json:"display_title,omitempty"`
	Status       string   `json:"status"`
	Branch       string   `json:"branch"`
	Program      string   `json:"program"`
	Path         string   `json:"path"`
	WorkDir      string   `json:"work_dir"`
	GitMode      bool     `json:"git_mode"`
	CreatedAt    string   `json:"created_at"`
	DiffAdded    int      `json:"diff_added"`
	DiffRemoved  int      `json:"diff_removed"`
	Parent       string   `json:"parent,omitempty"`
	Children     []string `json:"children,omitempty"`
	InstanceType string   `json:"instance_type,omitempty"`
	AgentPreset  string   `json:"agent_preset,omitempty"`
	MCPPort      int      `json:"mcp_port,omitempty"`
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
		Title:        inst.Title,
		DisplayTitle: inst.DisplayTitle,
		Status:       statusString(inst.Status),
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
	j.Parent = inst.Parent
	j.Children = inst.Children
	j.InstanceType = inst.InstanceType
	j.AgentPreset = inst.AgentPreset
	j.MCPPort = inst.MCPPort
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
		changed := make(map[string]*AgentMeta) // title -> new metadata

		for _, inst := range s.instances {
			var meta *AgentMeta

			if inst.Started() && !inst.Paused() {
				// Loop instances: check the orchestrator's /status endpoint for actual state
				if inst.InstanceType == "loop" && inst.MCPPort > 0 {
					s.pollLoopStatus(inst)
					meta = &AgentMeta{Status: statusString(inst.Status)}
				} else if inst.InstanceType == "loop" {
					inst.SetStatus(session.Running)
					meta = &AgentMeta{Status: "running"}
				} else {
					inst.CheckAndHandleTrustPrompt()
					updated, prompt, paneContent := inst.HasUpdated()

					// Parse pane for interactive prompts
					var panePrompt *PanePrompt
					if paneContent != "" {
						panePrompt = parsePaneActions(paneContent)
					}

					var statusStr string
					if updated {
						inst.SetStatus(session.Running)
						statusStr = "running"
					} else if prompt {
						// Known interactive prompt (Claude permission dialog, etc.)
						// For loop children, don't auto-dismiss — let the loop handle it
						if inst.Parent != "" {
							inst.SetStatus(session.Ready)
							statusStr = "waiting"
						} else {
							inst.TapEnter()
							statusStr = statusString(inst.Status)
						}
					} else {
						inst.SetStatus(session.Ready)
						statusStr = "ready"
					}
					_ = inst.UpdateDiffStats()

					meta = &AgentMeta{
						Status: statusStr,
						Prompt: panePrompt,
					}
				}
			} else {
				meta = &AgentMeta{Status: statusString(inst.Status)}
			}

			// Check if metadata changed
			s.statusMu.Lock()
			prev := s.lastAgentMeta[inst.Title]
			if prev == nil || !agentMetaEqual(prev, meta) {
				s.lastAgentMeta[inst.Title] = meta
				changed[inst.Title] = meta
			}
			s.statusMu.Unlock()
		}
		s.mu.Unlock()

		// Broadcast any changes
		if len(changed) > 0 {
			s.broadcastAgentMeta(changed)
		}
	}
}

// agentMetaEqual compares two AgentMeta for equality.
func agentMetaEqual(a, b *AgentMeta) bool {
	if a.Status != b.Status {
		return false
	}
	if (a.Prompt == nil) != (b.Prompt == nil) {
		return false
	}
	if a.Prompt != nil && b.Prompt != nil {
		if a.Prompt.Message != b.Prompt.Message {
			return false
		}
		if len(a.Prompt.Actions) != len(b.Prompt.Actions) {
			return false
		}
		for i := range a.Prompt.Actions {
			if a.Prompt.Actions[i] != b.Prompt.Actions[i] {
				return false
			}
		}
	}
	return true
}

// loopStatusClient is a shared HTTP client with short timeout for polling loop status.
var loopStatusClient = &http.Client{Timeout: 500 * time.Millisecond}

// pollLoopStatus checks the orchestrator binary's /status endpoint and maps
// its state to an instance status.
func (s *Server) pollLoopStatus(inst *session.Instance) {
	statusURL := fmt.Sprintf("http://localhost:%d/status", inst.MCPPort)
	resp, err := loopStatusClient.Get(statusURL)
	if err != nil {
		// MCP server not up yet — show as waiting (ready)
		inst.SetStatus(session.Ready)
		return
	}
	defer resp.Body.Close()

	var result struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		inst.SetStatus(session.Ready)
		return
	}

	switch result.State {
	case "running":
		inst.SetStatus(session.Running)
	case "paused":
		inst.SetStatus(session.Paused)
	case "idle", "done":
		inst.SetStatus(session.Ready)
	default:
		inst.SetStatus(session.Ready)
	}
}

// broadcastAgentMeta sends enriched status updates to all connected status WS clients.
// Includes both the new "agents" format and backwards-compatible "statuses" map.
func (s *Server) broadcastAgentMeta(changed map[string]*AgentMeta) {
	// Build backwards-compatible statuses map
	statuses := make(map[string]string, len(changed))
	for title, meta := range changed {
		statuses[title] = meta.Status
	}

	msg := map[string]interface{}{
		"type":     "status_update",
		"statuses": statuses,
		"agents":   changed,
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

	// Collect current state, register client, and send initial state
	// under statusMu to prevent concurrent writes and missed updates.
	s.mu.RLock()
	currentStatuses := make(map[string]string)
	currentAgents := make(map[string]*AgentMeta)
	for _, inst := range s.instances {
		status := statusString(inst.Status)
		currentStatuses[inst.Title] = status
		currentAgents[inst.Title] = &AgentMeta{Status: status}
	}
	s.mu.RUnlock()

	s.statusMu.Lock()
	s.statusClients[conn] = true
	if len(currentStatuses) > 0 {
		if err := conn.WriteJSON(map[string]interface{}{
			"type":     "status_update",
			"statuses": currentStatuses,
			"agents":   currentAgents,
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
// It uses an O(1) history_size metadata query to detect new scrollback, then captures only the
// delta instead of the entire scrollback buffer every tick.
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

			paneContent, err := inst.Preview()
			if err != nil {
				continue
			}

			historySize, err := inst.GetHistorySize()
			if err != nil {
				continue
			}

			var newScrollback string
			lastSize := s.lastHistorySize[inst.Title]

			// Resize guard: if history_size decreased (e.g. terminal resize
			// caused line rewrapping), reset our tracker. The overlap between
			// stable and pane is handled at read time by findOverlap in GetState.
			if historySize < lastSize {
				s.lastHistorySize[inst.Title] = historySize
			} else if historySize > lastSize {
				delta := historySize - lastSize
				newScrollback, err = inst.CaptureScrollback(delta)
				if err != nil {
					continue
				}
				s.lastHistorySize[inst.Title] = historySize
			}

			cl.Ingest(newScrollback, paneContent)
		}
		s.mu.RUnlock()
	}
}

func lastNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

func firstNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

func (s *Server) handleReorderInstances(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Order []string `json:"order"` // titles in desired order
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Build a map of title -> instance for quick lookup
	byTitle := make(map[string]*session.Instance, len(s.instances))
	for _, inst := range s.instances {
		byTitle[inst.Title] = inst
	}

	// Reorder: place titles from body.Order first, then any remaining
	reordered := make([]*session.Instance, 0, len(s.instances))
	seen := make(map[string]bool)
	for _, title := range body.Order {
		if inst, ok := byTitle[title]; ok && !seen[title] {
			reordered = append(reordered, inst)
			seen[title] = true
		}
	}
	// Append any instances not in the order list (safety net)
	for _, inst := range s.instances {
		if !seen[inst.Title] {
			reordered = append(reordered, inst)
		}
	}

	s.instances = reordered
	if err := s.save(); err != nil {
		log.ErrorLog.Printf("failed to save after reorder: %v", err)
	}
	w.WriteHeader(http.StatusOK)
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

// ensureLocalPlansDir creates a .claude/settings.local.json in the working directory
// that configures Claude CLI to store plan files locally (colocated with the conversation)
// rather than in the global ~/.claude/plans/ directory.
func ensureLocalPlansDir(workDir string) {
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	// If settings.local.json already exists, merge plansDirectory into it
	existing := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &existing)
	}
	if _, ok := existing["plansDirectory"]; ok {
		return // already configured
	}

	existing["plansDirectory"] = "./.claude/plans"

	if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0755); err != nil {
		log.ErrorLog.Printf("failed to create .claude dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		log.ErrorLog.Printf("failed to marshal settings: %v", err)
		return
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0644); err != nil {
		log.ErrorLog.Printf("failed to write settings.local.json: %v", err)
	}
}

// waitForMCPServer polls the MCP URL until it responds or the timeout expires.
// This ensures the MCP HTTP server is accepting connections before the leader
// Claude session starts, so Claude's MCP initialization handshake succeeds.
func waitForMCPServer(mcpURL string, timeout time.Duration) {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.Get(mcpURL + "/status")
		if err == nil {
			resp.Body.Close()
			log.InfoLog.Printf("[MCP] Server at %s is reachable", mcpURL)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	log.WarningLog.Printf("[MCP] Timed out waiting for server at %s after %v", mcpURL, timeout)
}

// writeMCPConfig writes a .mcp.json in workDir so the Claude CLI connects
// to the orchestrator's MCP HTTP server.
func writeMCPConfig(workDir, mcpURL string) {
	mcpPath := filepath.Join(workDir, ".mcp.json")

	config := map[string]interface{}{
		"mcpServers": map[string]interface{}{
			"orchestrator": map[string]interface{}{
				"type": "http",
				"url":  mcpURL,
			},
		},
	}

	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		log.ErrorLog.Printf("[MCP] failed to marshal mcp config: %v", err)
		return
	}
	if err := os.WriteFile(mcpPath, append(data, '\n'), 0644); err != nil {
		log.ErrorLog.Printf("[MCP] failed to write .mcp.json: %v", err)
		return
	}
	log.InfoLog.Printf("[MCP] Wrote %s → %s", mcpPath, mcpURL)
}

// applyPresetSettings merges the preset's default permissions into settings.local.json.
func applyPresetSettings(workDir string, preset orchestrator.AgentPreset) {
	settingsPath := filepath.Join(workDir, ".claude", "settings.local.json")

	// Load existing settings
	existing := make(map[string]interface{})
	if data, err := os.ReadFile(settingsPath); err == nil {
		json.Unmarshal(data, &existing)
	}

	// Parse preset settings
	var presetData map[string]interface{}
	if err := json.Unmarshal([]byte(preset.DefaultSettings()), &presetData); err != nil {
		log.ErrorLog.Printf("failed to parse preset settings for %s: %v", preset, err)
		return
	}

	// Merge preset into existing (preset values override)
	for k, v := range presetData {
		existing[k] = v
	}

	if err := os.MkdirAll(filepath.Join(workDir, ".claude"), 0755); err != nil {
		log.ErrorLog.Printf("failed to create .claude dir: %v", err)
		return
	}
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		log.ErrorLog.Printf("failed to marshal settings: %v", err)
		return
	}
	if err := os.WriteFile(settingsPath, append(data, '\n'), 0644); err != nil {
		log.ErrorLog.Printf("failed to write settings.local.json: %v", err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (s *Server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string `json:"title"`
		Prompt      string `json:"prompt"`
		Path        string `json:"path"`
		GitMode     bool   `json:"git_mode"`
		RepoPath    string `json:"repo_path"`
		Branch      string `json:"branch"`
		Parent       string `json:"parent,omitempty"`
		AgentPreset  string `json:"agent_preset,omitempty"`
		InstanceType string `json:"instance_type,omitempty"`
		MCPPort      int    `json:"-"` // internal use, not from request body
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

	// Determine the program to run: loop instances run the orchestrator binary,
	// regular instances run the configured CLI program (e.g. claude).
	program := s.program
	if body.InstanceType == "loop" {
		// Find the orchestrator binary: check bin/ next to the server binary first,
		// then fall back to same directory as the server binary.
		selfBin, err := os.Executable()
		if err != nil {
			selfBin = "."
		}
		baseDir := filepath.Dir(selfBin)
		orchBin := filepath.Join(baseDir, "claude-squad-orchestrator")
		if binPath := filepath.Join(baseDir, "bin", "claude-squad-orchestrator"); fileExists(binPath) {
			orchBin = binPath
		}
		mcpPort := s.nextMCPPort
		s.nextMCPPort++
		escaped := strings.ReplaceAll(body.Title, "'", "'\\''")
		program = fmt.Sprintf("%s --group '%s' --base-url http://localhost:%d --mcp-port %d", orchBin, escaped, s.port, mcpPort)
		if body.Prompt != "" {
			escapedTask := strings.ReplaceAll(body.Prompt, "'", "'\\''")
			program += fmt.Sprintf(" --task '%s'", escapedTask)
		}
		// Store the MCP port so child instances can find it
		body.MCPPort = mcpPort
		log.InfoLog.Printf("[Orchestrator] Creating loop instance %q on MCP port %d with binary: %s", body.Title, mcpPort, orchBin)
	}

	// Ensure the working directory exists
	if err := os.MkdirAll(body.Path, 0755); err != nil {
		http.Error(w, fmt.Sprintf("failed to create working directory: %v", err), http.StatusInternalServerError)
		return
	}

	inst, err := session.NewInstance(session.InstanceOptions{
		Title:    body.Title,
		Path:     body.Path,
		RepoPath: body.RepoPath,
		Program:  program,
		AutoYes:  s.autoYes,
		GitMode:  body.GitMode,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Set orchestrator fields if provided
	if body.AgentPreset != "" {
		inst.AgentPreset = body.AgentPreset
	}
	if body.InstanceType != "" {
		inst.InstanceType = body.InstanceType
	}
	if body.MCPPort != 0 {
		inst.MCPPort = body.MCPPort
	}
	if body.Parent != "" {
		inst.Parent = body.Parent
		// Add to parent's children list
		if parent := s.findInstance(body.Parent); parent != nil {
			parent.Children = append(parent.Children, body.Title)
		}
	}

	// Ensure .claude/settings.local.json exists with local plans directory
	ensureLocalPlansDir(inst.Path)

	// Apply preset settings (permissions) to settings.local.json
	if body.AgentPreset != "" {
		applyPresetSettings(inst.Path, orchestrator.AgentPreset(body.AgentPreset))
	}

	// Write .mcp.json for orchestrator leader BEFORE Start() so Claude sees it at init.
	// Also wait for the MCP HTTP server to be reachable so Claude can connect.
	if body.AgentPreset == "orchestrator" && body.Parent != "" {
		if parent := s.findInstance(body.Parent); parent != nil && parent.MCPPort != 0 {
			mcpURL := fmt.Sprintf("http://localhost:%d", parent.MCPPort)
			log.InfoLog.Printf("[Orchestrator] Writing MCP config for leader %q → %s", body.Title, mcpURL)
			writeMCPConfig(inst.Path, mcpURL)

			// Wait for MCP server to be reachable before starting the leader,
			// so Claude's MCP init handshake doesn't fail.
			waitForMCPServer(mcpURL, 10*time.Second)
		} else {
			log.WarningLog.Printf("[Orchestrator] Parent %q has no MCP port, skipping .mcp.json for leader %q", body.Parent, body.Title)
		}
	}

	if err := inst.Start(true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// For git worktree mode, also write .mcp.json into the worktree directory
	if body.AgentPreset == "orchestrator" && body.Parent != "" {
		if wt := inst.GetWorktreePath(); wt != "" {
			if parent := s.findInstance(body.Parent); parent != nil && parent.MCPPort != 0 {
				mcpURL := fmt.Sprintf("http://localhost:%d", parent.MCPPort)
				log.InfoLog.Printf("[Orchestrator] Writing MCP config to worktree for leader %q → %s", body.Title, mcpURL)
				writeMCPConfig(wt, mcpURL)
			}
		}
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
	// Use RawPath to preserve %2F in titles that contain slashes.
	rawPath := r.URL.RawPath
	if rawPath == "" {
		rawPath = r.URL.Path
	}
	path := strings.TrimPrefix(rawPath, "/api/instances/")
	// The action is always the last path segment (kill, send, ws, etc.)
	var encodedTitle, action string
	if lastSlash := strings.LastIndex(path, "/"); lastSlash >= 0 {
		encodedTitle = path[:lastSlash]
		action = path[lastSlash+1:]
	} else {
		encodedTitle = path
	}
	title, _ := url.PathUnescape(encodedTitle)
	if title == "" {
		http.Error(w, "missing instance title", http.StatusBadRequest)
		return
	}

	// For task action, proxy to the loop's MCP server (avoids tmux/pty issues with long text).
	if action == "task" {
		s.mu.RLock()
		inst := s.findInstance(title)
		s.mu.RUnlock()
		if inst == nil {
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}
		if inst.MCPPort == 0 {
			http.Error(w, "instance has no MCP server", http.StatusBadRequest)
			return
		}
		mcpURL := fmt.Sprintf("http://localhost:%d/task", inst.MCPPort)
		resp, err := http.Post(mcpURL, "application/json", r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	// For send/keys, use minimal locking — only hold the lock to find the
	// instance and convlog, then release before doing the actual tmux I/O.
	// This prevents blocking on pollMetadata/pollOutput which hold the lock
	// while running tmux subprocesses across all instances.
	if action == "send" || action == "keys" {
		s.mu.RLock()
		inst := s.findInstance(title)
		cl := s.convLogs[title]
		s.mu.RUnlock()

		if inst == nil {
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}

		if action == "send" {
			var body struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			log.InfoLog.Printf("[send] sending prompt to %q: %q", title, body.Text)
			if err := inst.SendPrompt(body.Text); err != nil {
				log.ErrorLog.Printf("[send] error sending to %q: %v", title, err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			log.InfoLog.Printf("[send] successfully sent to %q", title)
			if cl != nil {
				cl.AddInput(body.Text)
				cl.SetLastInput(body.Text)
			}
		} else {
			var body struct {
				Keys string `json:"keys"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := inst.SendKeys(body.Keys); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	inst := s.findInstance(title)
	if inst == nil {
		http.Error(w, "instance not found", http.StatusNotFound)
		return
	}

	switch action {

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
		// Cascade: if this is a loop/group, kill all children first
		if len(inst.Children) > 0 {
			for _, childTitle := range inst.Children {
				if child := s.findInstance(childTitle); child != nil {
					if err := child.Kill(); err != nil {
						log.ErrorLog.Printf("kill child %s (non-fatal): %v", childTitle, err)
					}
				}
				// Remove child from all tracking
				for i, existing := range s.instances {
					if existing.Title == childTitle {
						s.instances = append(s.instances[:i], s.instances[i+1:]...)
						break
					}
				}
				delete(s.convLogs, childTitle)
				delete(s.lastHistorySize, childTitle)
				s.statusMu.Lock()
				delete(s.lastAgentMeta, childTitle)
				s.statusMu.Unlock()
				_ = s.storage.DeleteInstance(childTitle)
			}
		}
		// Best-effort kill — continue with removal even if tmux session is already gone
		if err := inst.Kill(); err != nil {
			log.ErrorLog.Printf("kill %s (non-fatal): %v", title, err)
		}
		// Remove from list
		for i, existing := range s.instances {
			if existing.Title == title {
				s.instances = append(s.instances[:i], s.instances[i+1:]...)
				break
			}
		}
		delete(s.convLogs, title)
		delete(s.lastHistorySize, title)
		s.statusMu.Lock()
		delete(s.lastAgentMeta, title)
		s.statusMu.Unlock()
		_ = s.storage.DeleteInstance(title)
		w.WriteHeader(http.StatusOK)

	case "rename":
		var body struct {
			DisplayTitle string `json:"display_title"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		inst.DisplayTitle = body.DisplayTitle
		if err := s.save(); err != nil {
			log.ErrorLog.Printf("failed to save after rename: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.toJSON(inst))

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
		stableLines, stableSeqNo, pane, lastInput := cl.GetState()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"stable_lines":  stableLines,
			"stable_seq_no": stableSeqNo,
			"stable_count":  len(stableLines),
			"pane":          pane,
			"last_input":    lastInput,
		})

	case "input-history":
		cl := s.getOrCreateConvLog(title)
		history := cl.GetInputHistory()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"history": history,
		})

	case "diff":
		workDir := inst.GetWorktreePath()
		if workDir == "" {
			workDir = inst.Path
		}
		// Run git diff in the working directory
		diffCmd := exec.Command("git", "diff")
		diffCmd.Dir = workDir
		diffOutput, _ := diffCmd.Output()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"content": string(diffOutput),
		})

	case "git-status":
		workDir := inst.GetWorktreePath()
		if workDir == "" {
			workDir = inst.Path
		}

		// Check if this is a git repo
		checkCmd := exec.Command("git", "rev-parse", "--git-dir")
		checkCmd.Dir = workDir
		if err := checkCmd.Run(); err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"is_git":  false,
				"status":  "",
				"branch":  "",
			})
			return
		}

		// Get branch
		branchCmd := exec.Command("git", "branch", "--show-current")
		branchCmd.Dir = workDir
		branchOutput, _ := branchCmd.Output()

		// Get status
		statusCmd := exec.Command("git", "status", "--short")
		statusCmd.Dir = workDir
		statusOutput, _ := statusCmd.Output()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"is_git":  true,
			"status":  string(statusOutput),
			"branch":  strings.TrimSpace(string(branchOutput)),
		})

	case "memory":
		// Claude CLI stores memory at ~/.claude/projects/{encoded-path}/memory/
		// where encoded-path is the original repo path with "/" replaced by "-"
		// Always use inst.Path (the original repo), not the worktree path,
		// because Claude CLI keys memory on the original working directory.
		stripped := strings.TrimPrefix(inst.Path, "/")
		stripped = strings.ReplaceAll(stripped, "/", "-")
		stripped = strings.ReplaceAll(stripped, "_", "-")
		encodedPath := "-" + stripped
		homeDir, _ := os.UserHomeDir()
		memoryDir := filepath.Join(homeDir, ".claude", "projects", encodedPath, "memory")
		log.InfoLog.Printf("memory lookup: inst.Path=%s memoryDir=%s", inst.Path, memoryDir)

		if r.Method == http.MethodGet {
			type memoryFile struct {
				Name    string `json:"name"`
				Path    string `json:"path"`
				Content string `json:"content"`
			}

			var result []memoryFile
			entries, err := os.ReadDir(memoryDir)
			if err == nil {
				for _, e := range entries {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
						continue
					}
					p := filepath.Join(memoryDir, e.Name())
					content, err := os.ReadFile(p)
					if err != nil {
						continue
					}
					result = append(result, memoryFile{
						Name:    e.Name(),
						Path:    p,
						Content: string(content),
					})
				}
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"files": result, "directory": memoryDir})

		} else if r.Method == http.MethodPut {
			var body struct {
				Path    string `json:"path"`
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

			absPath, err := filepath.Abs(body.Path)
			if err != nil || !strings.HasPrefix(absPath, memoryDir) {
				http.Error(w, "invalid path", http.StatusBadRequest)
				return
			}

			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(absPath, []byte(body.Content), 0644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "path": absPath})
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "plans":
		workDir := inst.GetWorktreePath()
		if workDir == "" {
			workDir = inst.Path
		}
		plansDir := filepath.Join(workDir, ".claude", "plans")

		if r.Method == http.MethodGet {
			type planFile struct {
				Name    string `json:"name"`
				Path    string `json:"path"`
				Content string `json:"content"`
			}

			var result []planFile
			entries, err := os.ReadDir(plansDir)
			if err == nil {
				for _, e := range entries {
					if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
						continue
					}
					p := filepath.Join(plansDir, e.Name())
					content, err := os.ReadFile(p)
					if err != nil {
						continue
					}
					result = append(result, planFile{
						Name:    e.Name(),
						Path:    p,
						Content: string(content),
					})
				}
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"files": result})

		} else if r.Method == http.MethodPut {
			var body struct {
				Path    string `json:"path"`
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

			absPath, err := filepath.Abs(body.Path)
			if err != nil || !strings.HasPrefix(absPath, plansDir) {
				http.Error(w, "invalid path", http.StatusBadRequest)
				return
			}

			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(absPath, []byte(body.Content), 0644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "path": absPath})
		} else {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "rules":
		workDir := inst.GetWorktreePath()
		if workDir == "" {
			workDir = inst.Path
		}

		if r.Method == http.MethodGet {
			// Return all config files: CLAUDE.md + settings files
			type configFile struct {
				Name     string `json:"name"`
				Path     string `json:"path"`
				Content  string `json:"content"`
				Exists   bool   `json:"exists"`
				Writable bool   `json:"writable"`
			}

			files := []struct {
				name     string
				path     string
				writable bool
			}{
				{"CLAUDE.md", filepath.Join(workDir, "CLAUDE.md"), true},
				{".claude/settings.json", filepath.Join(workDir, ".claude", "settings.json"), true},
				{".claude/settings.local.json", filepath.Join(workDir, ".claude", "settings.local.json"), true},
				{".mcp.json", filepath.Join(workDir, ".mcp.json"), true},
			}

			var result []configFile
			for _, f := range files {
				cf := configFile{Name: f.name, Path: f.path, Writable: f.writable}
				content, err := os.ReadFile(f.path)
				if err == nil {
					cf.Content = string(content)
					cf.Exists = true
				}
				result = append(result, cf)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"files": result})

		} else if r.Method == http.MethodPut {
			var body struct {
				Path    string `json:"path"`
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

			// Validate the path is within the workdir
			absPath, err := filepath.Abs(body.Path)
			if err != nil || !strings.HasPrefix(absPath, workDir) {
				http.Error(w, "invalid path", http.StatusBadRequest)
				return
			}

			// Create parent directory if needed
			if err := os.MkdirAll(filepath.Dir(absPath), 0755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.WriteFile(absPath, []byte(body.Content), 0644); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "path": absPath})
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

	// Seed from current raw stable count so the WS only sends lines added
	// after the client fetched /history. We use the raw (untrimmed) count
	// so that overlap fluctuations between stable and pane don't cause
	// previously-sent lines to be re-sent as "new" history_append.
	lastRawStableCount := cl.GetRawStableCount()
	var lastPaneContent string
	var lastLastInput string

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
		}

		// Use raw stable count to detect genuinely new lines (immune to overlap changes)
		rawStableCount := cl.GetRawStableCount()
		_, _, pane, lastInput := cl.GetState()

		s.mu.RLock()
		status := statusString(inst.Status)
		s.mu.RUnlock()

		// Send new stable lines as history_append.
		// New scrollback lines have already left the pane by the time they appear
		// in stable, so no overlap filtering is needed here. The pane message
		// (sent below) replaces the volatile content independently.
		if rawStableCount > lastRawStableCount {
			newLines := cl.GetStableSince(lastRawStableCount)
			lastRawStableCount = rawStableCount
			if len(newLines) > 0 {
				msg := map[string]interface{}{
					"type":  "history_append",
					"lines": newLines,
				}
				if err := conn.WriteJSON(msg); err != nil {
					return
				}
			}
		}

		// Send current pane (volatile) - only if changed
		paneContent := strings.Join(pane, "\n")
		if paneContent != lastPaneContent || lastInput != lastLastInput {
			lastPaneContent = paneContent
			lastLastInput = lastInput
			msg := map[string]interface{}{
				"type":       "pane",
				"content":    paneContent,
				"status":     status,
				"last_input": lastInput,
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
	delete(s.lastHistorySize, title)
	s.statusMu.Lock()
	delete(s.lastAgentMeta, title)
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

	srv.port = port
	srv.nextMCPPort = port + 100 // MCP ports start at webserver port + 100
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
	mux.HandleFunc("/api/instances/reorder", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		srv.handleReorderInstances(w, r)
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

