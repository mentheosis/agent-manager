# Web UI Architecture

> **Keep this document up to date.** When adding endpoints, changing data flow, adding files, or making design decisions in `webserver/`, update the relevant sections below.

## Overview

The web UI provides a browser-based interface for managing Claude Code sessions that are running inside tmux. It mirrors what the TUI does but over HTTP/WebSocket.

## File Layout

```
webserver/
  server.go          # HTTP server, API routes, WebSocket handlers, polling loops
  convlog.go         # ConversationLog: accumulates terminal output into stable/pane
  convlog_test.go    # Tests for ConversationLog
  clisessions.go     # Discovery of standalone Claude CLI sessions
  static/
    index.html       # Single-page app: all HTML, CSS, and main JS
    js/
      api.js         # fetch wrapper, base URL = /api/instances
      convlog.js     # ConvLogView: manages WS connection + renders history/pane
      input-history.js  # InputHistory: prompt input with history recall
      ansi.js        # ANSI escape → HTML conversion
```

## Data Flow: Terminal Output → Browser

```
tmux pane ──poll (200ms)──► ConversationLog ──WS (200ms)──► ConvLogView (browser)
```

### 1. Capture (server.go `pollOutput`)

Every 200ms, for each active instance:
- Capture visible pane via `inst.Preview()`
- Query `inst.GetHistorySize()` to detect new scrollback (O(1) metadata check)
- If scrollback grew, capture only the delta via `inst.CaptureScrollback(delta)`
- Feed both into `cl.Ingest(newScrollback, paneContent)`

This delta approach avoids re-reading the entire scrollback buffer every tick.

### 2. Accumulation (convlog.go `ConversationLog`)

ConversationLog maintains two regions:
- **stableLines**: append-only history (lines that scrolled off the visible pane)
- **currentPane**: volatile visible pane content (replaced every ingest)

Lines enter stable **only** via scrollback promotion — when tmux reports new scrollback lines, they're appended to stableLines. There is no turn-based promotion (promoting pane to stable on status transitions was tried but caused duplication).

The struct also tracks:
- `lastStatus`: current instance status (for transition detection)
- `lastInput`: most recent user prompt (displayed in UI)
- `inputHistory`: all prompts sent by user

**Nothing is persisted to disk.** On server restart, ConversationLog starts empty and rebuilds from whatever is in the tmux scrollback buffer.

### 3. Delivery to Browser

Two mechanisms:
- **REST `GET /history`**: returns full `stable_lines` + `pane` + `last_input` (used on initial page load)
- **WebSocket `/ws`**: ticks every 200ms, sends:
  - `history_append` messages when stable lines grow (delta only, from `lastStableCount`)
  - `pane` messages when pane content changes (includes `status`, `last_input`)

### 4. Rendering (convlog.js `ConvLogView`)

The browser maintains two DOM regions:
- `#output-history` (historyDiv): stable content, appended via `history_append`
- `#output-live` (paneDiv): volatile pane, replaced on each `pane` message
- `.last-input` (lastInputDiv): shows most recent user prompt below the pane

A `generation` counter prevents stale callbacks when switching between instances.

## Interactive Prompt Detection

The pane content is parsed (ANSI-stripped) on every update to detect Claude CLI interactive prompts:

- **Numbered options**: regex `^\s*[❯>]?\s*(\d+)\.\s+(.+)$` → buttons that send the digit keystroke
- **Hint shortcuts**: regex `(Esc|Tab|ctrl+\w)\s+to\s+(\w+)` → buttons that send the corresponding key
  - Esc → `\x1b`, Tab → `\x1b[Z` (shift+tab), ctrl+X → ctrl character

These buttons appear in `#action-bar` between the output area and input area. They call `POST /api/instances/{title}/keys` which writes raw bytes to the tmux PTY.

## Key API Endpoints

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/api/instances` | GET | List all instances |
| `/api/instances` | POST | Create new instance |
| `/api/instances/{title}/send` | POST | Send prompt (text + Enter) |
| `/api/instances/{title}/keys` | POST | Send raw keystrokes to tmux |
| `/api/instances/{title}/history` | GET | Get conversation state |
| `/api/instances/{title}/ws` | GET | WebSocket for live updates |
| `/api/instances/{title}/diff` | GET | Git diff for instance |
| `/api/instances/{title}/rules` | GET/PUT | Read/write config files (CLAUDE.md, settings) |
| `/api/instances/reorder` | POST | Reorder instance list (persisted) |
| `/api/statuses/ws` | GET | WebSocket for status broadcasts |
| `/api/sessions` | GET | List all saved sessions (from state.json) |

## Instance List

- Sidebar shows all instances, drag-to-reorder supported
- Order is persisted via `POST /api/instances/reorder` → saves to `state.json`
- Status updates arrive via a dedicated status WebSocket (`/api/statuses/ws`)

## Status Polling (server.go `pollMetadata`)

Every 500ms, checks each active instance for activity:
- If tmux content changed → status = "running"
- If unchanged and has a prompt → auto-tap Enter (for trust prompts)
- If unchanged and no prompt → status = "ready"
- Status changes broadcast to all connected status WS clients

## Design Decisions

1. **No persistence for conversation logs**: ConversationLog is purely in-memory. Tmux scrollback is the source of truth. This avoids stale data and keeps the server simple.

2. **Scrollback-only promotion**: Lines only move to stable history when they physically scroll off the visible pane. Turn-based promotion (promoting on running→ready transitions) was removed because it caused duplicate lines appearing in both stable and pane.

3. **Delta scrollback capture**: Instead of capturing the full scrollback buffer every tick, we track `lastHistorySize` and only capture new lines. This is O(delta) instead of O(total).

4. **Raw keystroke API**: The `/keys` endpoint sends bytes directly to the PTY, enabling the UI to interact with Claude CLI's interactive prompts (numbered choices, Esc/Tab/ctrl shortcuts) without the server needing to understand the prompt format.

5. **Single HTML file**: All CSS and main application JS live in `index.html`. Only reusable modules (ConvLogView, InputHistory, API wrapper, ANSI parser) are in separate JS files.
