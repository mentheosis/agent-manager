# Orchestrator Feature Implementation Plan

## Overview

Port the orchestration feature from claude-squad to agent-manager. This enables a "leader" agent to coordinate multiple "worker" agents via MCP tools, with the UI supporting team management, YAML configuration, and drag-and-drop agent assignment.

---

## Architecture Summary

### Key Differences from claude-squad

The agent-manager uses the **Claude Agent SDK** instead of raw tmux sessions. This provides:
- **Structured events** instead of terminal scraping (status, tool_use, tool_result, etc.)
- **No interactive prompts** - SDK's `permission_mode` controls permissions at session level
- **Cleaner API** - HTTP endpoints for send/status/history instead of tmux keystroke injection

### Components

1. **Orchestrator Binary** (`am-orchestrator`) - Standalone Go process that:
   - Runs a control loop monitoring agent statuses via WebSocket
   - Exposes MCP server with tools for the leader agent
   - Monitors for permission errors and escalates to user when needed

2. **MCP Tools** (5 tools for leader agent - updated for SDK):
   - `list_agents` - List sub-agents with status/directory/preset
   - `send_to_agent` - Send prompt to specific agent
   - `read_agent_output(agent, count=20, offset=0, types=None)` - Read agent's recent events
   - `get_agent_status` - Check if agent is ready/running/error
   - `mark_task_done` - Signal completion, pause loop
   
   **read_agent_output params:**
   - `count`: Number of events to return (default: 20)
   - `offset`: Skip N events from end before reading (for scrolling back)
   - `types`: Optional filter (e.g., ["assistant_text", "tool_result"])
   
   Leader decides how much context it needs and can paginate backwards.

   **Removed:** `respond_to_prompt` (no interactive prompts with SDK)
   
   **Permission handling:** If an agent encounters a permission error (visible in `tool_result` events with `is_error: true`), the leader can either:
   - Adjust the task to work within permissions
   - Report to user that manual intervention is needed

3. **Agent Presets** (behavioral, not permission-based):
   - `coder` - Instructions for code editing, testing, implementation
   - `researcher` - Instructions for read-only research, documentation
   - `orchestrator` - Instructions for team coordination via MCP tools
   
   **Note:** These are CLAUDE.md instruction files that guide agent behavior. Actual permissions are controlled by:
   - Session-level `permission_mode` (acceptEdits, plan, etc.)
   - Repo-level `.claude/settings.json` (allowed directories, tools, etc.)

4. **Instance Relationships**:
   - `InstanceType`: `"claude"` (normal) or `"loop"` (control loop)
   - `Parent`: Title of parent group/team
   - `Children`: List of child agent titles
   - `AgentPreset`: One of coder/researcher/orchestrator

---

## Dockerfile Build Strategy

To minimize rebuilds, use multi-stage build with proper layer separation:

```dockerfile
# Stage 1: Go dependencies (cached unless go.mod/go.sum change)
FROM golang:1.22-alpine AS go-deps
WORKDIR /build
COPY orchestrator/go.mod orchestrator/go.sum ./
RUN go mod download

# Stage 2: Go build (rebuilds only if orchestrator/ source changes)
FROM go-deps AS go-build
COPY orchestrator/ ./
RUN go build -o am-orchestrator .

# Stage 3: Python dependencies (existing, unchanged)
FROM python:3.12-slim AS python-deps
# ... existing pip install ...

# Stage 4: Final image
FROM python:3.12-slim
COPY --from=go-build /build/am-orchestrator /usr/local/bin/
COPY --from=python-deps ...
# ... rest of existing setup ...
```

**Layer order ensures:**
- Go dependency changes → rebuild go-deps + go-build only
- Go source changes → rebuild go-build only
- Python changes → don't touch Go layers
- Neither affects the other's dependency cache

---

## Implementation Phases

### Phase 1: Data Model Extensions

**Files to modify:**
- `src/agent_manager/instance.py`
- `src/agent_manager/persistence.py`
- `src/agent_manager/state.py`
- `src/agent_manager/server.py`

**Changes:**

1. Add fields to `Instance`:
   ```python
   instance_type: str = "claude"  # "claude" | "loop"
   parent: str | None = None
   children: list[str] = []
   agent_preset: str | None = None  # "coder" | "researcher" | "orchestrator"
   task: str | None = None  # Task description for loop instances
   ```

2. Add fields to `InstanceRecord` for persistence

3. Add API endpoints:
   - `POST /api/instances/{title}/reparent` - Move agent to new parent
   - `GET /api/instances/{title}/children` - Get child instances
   - Update `_summary()` to include new fields

4. Enhance history endpoint for tail-based reading (read backwards):
   ```
   GET /api/instances/{title}/history?tail=20&offset=0&types=assistant_text,tool_result
   ```
   - `tail`: number of events from the end (default: 50, max: 200)
   - `offset`: skip N events from end before taking tail (for pagination backwards)
   - `types`: comma-separated event types to include (optional filter)
   
   Event types: `assistant_text`, `thinking`, `tool_use`, `tool_result`, `user_prompt`, `status`, `error`
   
   Response: `{ events: [...], total_count: N, has_more: bool }`

5. Add validation:
   - Loop instances cannot be reparented
   - Orchestrator-preset agents cannot be moved out of their team

**Checklist:**
- [x] Add instance_type, parent, children, agent_preset, task to Instance dataclass
- [x] Add fields to InstanceRecord and persistence
- [x] Add reparent API endpoint with validation
- [x] Add task update endpoint for loop instances
- [x] Update _summary() to include new fields
- [x] Add children lookup helper
- [x] Add tail/offset/types params to history endpoint for backwards reading

---

### Phase 2: Orchestrator Binary (Go)

**New directory:** `orchestrator/` at repo root

**Files to create:**
- `orchestrator/main.go` - Entry point
- `orchestrator/loop.go` - Control loop
- `orchestrator/mcp.go` - MCP server (5 tools, no respond_to_prompt)
- `orchestrator/client.go` - HTTP client for agent-manager API
- `orchestrator/watcher.go` - WebSocket client for status updates
- `orchestrator/protocol.go` - Data structures
- `orchestrator/presets.go` - Embedded CLAUDE.md instruction files
- `orchestrator/go.mod` - Go module

**Port from claude-squad with SDK adaptations:**
- Copy and adapt the orchestrator package
- Update API URLs to match agent-manager endpoints:
  - `POST /api/instances/{title}/send` - send prompt
  - `GET /api/instances/{title}/history?tail=20&types=...` - read recent events (backwards)
  - `GET /api/instances/{title}` - get status
- Update WebSocket protocol to match agent-manager events:
  - `ws://host/api/instances/{title}/stream`
  - Events: `status`, `tool_use`, `tool_result`, `assistant_text`, etc.
- **Remove tmux-specific code** (keystroke injection, terminal scraping)
- **Remove respond_to_prompt tool** (not needed with SDK)
- **Add permission error detection** - scan `tool_result` events for `is_error: true`

**MCP Server modes:**
- stdio (for Claude Desktop)
- HTTP (for programmatic access + status endpoints)

**Preset files (embedded in binary):**
Each preset has a CLAUDE.md file with behavioral instructions:
```go
//go:embed presets/coder.md
var coderPreset string

//go:embed presets/researcher.md  
var researcherPreset string

//go:embed presets/orchestrator.md
var orchestratorPreset string
```
These get written to the agent's working directory when the team starts.

**Checklist:**
- [x] Create orchestrator/ directory structure
- [x] Port loop.go with control loop logic (remove tmux specifics)
- [x] Port mcp.go with 5 MCP tools (drop respond_to_prompt)
- [x] Port client.go, update for agent-manager HTTP API
- [x] Port watcher.go (polling-based status monitoring)
- [x] Port protocol.go data structures
- [x] Create presets/*.md files with embedded instructions
- [x] Create main.go entry point
- [x] Add go.mod with dependencies
- [x] Update Dockerfile with multi-stage Go build
- [ ] Test MCP tools work with Claude

---

### Phase 3: UI - Team Management Panel

**Files to modify:**
- `static/components/am-app.js`
- New: `static/components/am-team-panel.js`
- New: `static/components/am-team-panel.css`

**Team Panel Features:**
1. Side panel shown only when viewing a `loop` instance on Conversation tab
2. Contains:
   - Task prompt textarea (editable)
   - List of team member cards showing:
     - Status dot (ready/running/error)
     - Agent name
     - Preset badge (coder/researcher/orchestrator)
     - Remove from team button (X)
   - Error alerts section (agents with `is_error` tool results)
   - "Add Agent" button (opens dialog)
   - "Start/Pause/Resume" orchestration button

**Checklist:**
- [x] Create am-team-panel.js component
- [x] Create am-team-panel.css styles
- [x] Add panel to am-app.js layout (conditional on instance_type)
- [x] Implement team member cards with live status
- [ ] Implement error alerts UI (permission issues, etc.) - deferred to Phase 7
- [x] Add orchestration control buttons (start/pause/resume) - wired to stubs
- [x] Wire up to backend APIs (children, reparent, task)

---

### Phase 4: UI - YAML Configuration Modal

**Files to modify:**
- New: `static/components/am-team-config-dialog.js`
- `static/css/dialogs.css`

**YAML Schema:**
```yaml
title: my-team
path: /path/to/workspace
task: "Build feature X with tests"
agents:
  - name: coder-1
    path: /path/to/repo
    preset: coder
  - name: researcher
    path: /path/to/docs
    preset: researcher
```

**Features:**
- Textarea for YAML input
- Parse and validate on submit
- Create loop instance + child agents
- Auto-assign orchestrator preset to leader

**Checklist:**
- [x] Create am-team-config-dialog.js component
- [x] Add YAML parsing (simple custom parser for our schema)
- [x] Validate schema (title, path, agents required)
- [x] Create instances via API on submit
- [x] Add "New Team" button to sidebar header
- [x] Style the dialog
- [x] API: Add endpoint to set instance_type (PATCH /api/instances/{title}/type)
- [x] API: Add endpoint to set agent_preset (same endpoint handles both)

---

### Phase 5: UI - Drag and Drop for Team Assignment

**Files to modify:**
- `static/components/am-sidebar.js`
- `static/components/am-sidebar.css`

**Behavior:**
1. Dragging an agent over a loop instance header → highlight as drop target
2. Dropping INTO a loop → reparent agent to that team
3. Dropping BETWEEN items → reorder (existing behavior)
4. Prevent invalid operations:
   - Cannot drag loop instances
   - Cannot drag orchestrator-preset agents out of team
   - Cannot drag into non-loop instances

**Visual indicators:**
- Orange outline on valid drop target (loop instance)
- Red X cursor on invalid drop
- Blue line for reorder position (existing)

**Checklist:**
- [x] Update dragover to detect loop instance targets
- [x] Add .drop-target-group class for loop hover
- [x] Update drop handler to call reparent API
- [x] Add validation for drag restrictions
- [x] Add visual feedback for invalid operations
- [ ] Test drag into team, drag out of team, reorder within team

---

### Phase 6: UI - Loop Instance Display

**Files to modify:**
- `static/components/am-sidebar.js`
- `static/components/am-sidebar.css`
- `static/components/am-terminal-pane.js`

**Sidebar changes:**
- Loop instances show with orange left border
- Collapse/expand arrow to show/hide children
- Children indented under parent
- Child count badge

**Terminal changes:**
- Loop instances show orchestrator output differently
- Status bar shows loop state (idle/running/paused/done)

**Toolbar changes:**
- "Restart Loop" button for loop instances
- Hide Pause/Resume for now (or wire up)

**Checklist:**
- [x] Add visual distinction for loop instances in sidebar (orange border)
- [x] Implement collapse/expand for team children
- [x] Indent children under parent
- [x] Add "Restart Loop" button to toolbar (wired to stub)
- [x] Update status bar for loop state
- [x] Style loop-specific elements (preset badges, type badge in toolbar)

---

### Phase 7: Process Management

**Files to modify:**
- `src/agent_manager/instance.py`
- `src/agent_manager/state.py`
- New: `src/agent_manager/orchestrator.py`

**Features:**
1. When a loop instance is created, spawn the orchestrator binary
2. Pass configuration: group title, base URL, MCP port, task
3. Monitor process health
4. Restart on demand
5. Kill process when instance deleted

**Checklist:**
- [x] Create orchestrator.py for process management
- [x] Spawn orchestrator binary on loop instance creation (via API)
- [x] Track process PID and health
- [x] Implement restart functionality
- [x] Clean up process on instance deletion
- [x] Handle orchestrator stdout/stderr logging
- [x] Add API endpoints: start, stop, restart, status, output
- [x] Wire up UI buttons to orchestrator APIs

---

### Phase 8: Integration Testing

**Test scenarios:**
1. Create team via YAML config
2. Drag agent into team
3. Drag agent out of team
4. Start orchestration
5. Verify MCP tools work
6. Pause/resume orchestration
7. Delete team (cleanup children)

**Checklist:**
- [ ] Manual testing of all workflows
- [ ] Verify WebSocket status updates
- [ ] Verify MCP server connectivity
- [ ] Test error handling (invalid YAML, failed reparent, etc.)

---

## Dependencies

**Go dependencies (orchestrator):**
- `github.com/gorilla/websocket` - WebSocket client
- `gopkg.in/yaml.v3` - YAML parsing (if needed)

**JS dependencies (frontend):**
- None new (vanilla JS, possibly js-yaml via CDN)

**Docker:**
- Build orchestrator binary in Dockerfile
- Include in container image

---

## Resolved Decisions

1. **Orchestrator binary location**: Build into this container (separate later if needed)
2. **MCP port allocation**: Dynamic ports (base port + offset per team)
3. **Preset files**: Embed in Go binary using `//go:embed` (simpler distribution)
4. **Task persistence**: Store `task` field on Instance metadata (keeps everything together)
5. **Multi-level nesting**: No - a team is one control loop with leader + optional workers

---

## Estimated Effort

| Phase | Effort | Dependencies |
|-------|--------|--------------|
| Phase 1: Data Model | 2-3 hours | None |
| Phase 2: Orchestrator Binary | 4-6 hours | Phase 1 |
| Phase 3: Team Panel UI | 3-4 hours | Phase 1 |
| Phase 4: YAML Config Dialog | 2-3 hours | Phase 1 |
| Phase 5: Drag-and-Drop | 2-3 hours | Phase 1 |
| Phase 6: Loop Display | 2-3 hours | Phase 1 |
| Phase 7: Process Management | 3-4 hours | Phase 2 |
| Phase 8: Integration Testing | 2-3 hours | All |

**Total: ~20-30 hours**

---

## Next Steps

1. ✅ Review and refine this plan
2. ✅ All open questions resolved
3. ✅ Phase 1: Data Model Extensions
4. ✅ Phase 2: Orchestrator Binary (Go)
5. ✅ Phase 3: UI - Team Management Panel
6. ✅ Phase 4: UI - YAML Configuration Modal
7. ✅ Phase 5: UI - Drag and Drop for Team Assignment
8. ✅ Phase 6: UI - Loop Instance Display
9. ✅ Phase 7: Process Management
10. 🔄 Phase 8: Integration Testing - manual testing required
