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
- If scrollback shrank, reset `lastHistorySize` to the new value (see below)
- On first sight of an instance with existing scrollback, capture full scrollback to seed history (restart recovery)
- Feed both into `cl.Ingest(newScrollback, paneContent)`

This delta approach avoids re-reading the entire scrollback buffer every tick.

**Screen clear handling**: Claude CLI periodically clears the scrollback buffer (via `\033[3J` or similar) during screen redraws, causing `history_size` to drop dramatically (e.g. 900 → 100). When it then renders new content, `history_size` grows back and the delta is captured. This means some content is re-captured, but `Ingest()` deduplicates it at the content level (see below). The tracker resets on decrease so that genuinely new content scrolling off after the clear is not missed.

### 2. Accumulation (convlog.go `ConversationLog`)

ConversationLog maintains two regions:
- **stableLines**: append-only history (lines that scrolled off the visible pane)
- **currentPane**: volatile visible pane content (replaced every ingest)

Lines enter stable **only** via scrollback delta — when tmux reports new scrollback lines, they're appended to stableLines. There is no turn-based promotion (promoting pane to stable on status transitions was tried but caused duplication).

**Ingest-time deduplication**: Before appending new scrollback lines, `Ingest()` calls `findOverlap(stableLines, newLines)` to detect if the captured lines overlap with the tail of existing stable history. This catches re-captured content after screen clears. The overlap detection uses `normalizeLine()` which strips ANSI escape sequences and trailing whitespace before comparing, because Claude CLI re-renders the same text with different ANSI formatting after clears. A 3-line seed match is required to avoid false positives on blank lines.

**Read-time deduplication**: `GetState()` also calls `findOverlap` between the tail of stableLines and the head of currentPane. This handles terminal resize cases where lines move back from scrollback into the visible pane. The WebSocket handler uses `GetRawStableCount()` for delta tracking, which is immune to overlap fluctuations.

Short responses that never scroll off the visible pane remain in `currentPane` and are always visible to the client via pane messages. They enter stableLines naturally when new output eventually displaces them into scrollback.

The struct also tracks:
- `lastStatus`: current instance status (informational only, does not drive promotion)
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

The browser maintains two DOM regions per instance:
- `#output-history` (historyDiv): stable content, appended via `history_append`
- `#output-live` (paneDiv): volatile pane, replaced on each `pane` message
- `.last-input` (lastInputDiv): shows most recent user prompt below the pane

**Client-side caching**: ConvLogView keeps a per-instance cache (`this._cache`) of DOM nodes and WebSocket connections. When switching conversations:
- The current instance's DOM wrapper is detached (kept in memory) and its WS stays alive in the background, continuing to receive `history_append` and `pane` updates.
- If switching to a previously viewed instance, the cached DOM is reattached instantly — no REST fetch needed. A WS is reconnected only if the previous one died.
- First visit to an instance fetches full history via `GET /history`, renders it, and opens a WS.
- `evict(title)` removes a title from cache and closes its WS (called on kill/delete).

This means background conversations never lose lines, and switching between conversations is instant.

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
| `/api/instances/{title}/plans` | GET/PUT | Read/write plan files (.claude/plans/*.md) |
| `/api/instances/{title}/rename` | POST | Set display_title (UI-only rename) |
| `/api/instances/{title}/reparent` | POST | Move instance to different parent group |
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

1. **Title is the immutable primary key**: `Instance.Title` is used as the unique identifier everywhere — storage lookups, tmux session names (`claudesquad_{title}`), git worktree branch names, API route paths, WebSocket connections, and all server-side maps (`convLogs`, `lastStatuses`, `lastHistorySize`). It **cannot be renamed** without breaking all of these. To support user-facing renames, a separate `DisplayTitle` field exists that is purely cosmetic — the UI shows `display_title || title` in the sidebar. The internal title remains unchanged. Any new code that needs a unique instance identifier should use `Title`, never `DisplayTitle`.

2. **Local plan files**: New instances get a `.claude/settings.local.json` with `"plansDirectory": "./.claude/plans"` so that Claude CLI stores plan files colocated with the conversation rather than in the global `~/.claude/plans/`. The Plans tab reads/writes files from this local directory.

3. **No persistence for conversation logs**: ConversationLog is purely in-memory. Tmux scrollback is the source of truth. This avoids stale data and keeps the server simple.

4. **Scrollback-only stable lines**: Lines only move to stable history when they physically scroll off the visible pane (detected via tmux `history_size` delta). There is no turn-based promotion — status transitions do not affect stable lines. This eliminates a class of duplication bugs where promoted pane content would overlap with scrollback delta. Short responses that never scroll off remain in the pane and are visible to clients via pane messages until displaced.

5. **Delta scrollback capture with content-level dedup**: Instead of capturing the full scrollback buffer every tick, we track `lastHistorySize` and only capture new lines. This is O(delta) instead of O(total). When `history_size` decreases (screen clear or resize), the tracker resets so new content isn't missed. Re-captured content is deduplicated at ingest time by `findOverlap`, which normalizes lines (strips ANSI codes and trailing whitespace) before comparing — this is necessary because Claude CLI re-renders the same text with different ANSI formatting after screen clears.

6. **Paste detection disabled on tmux sessions**: Sessions are created with `paste-time 0` to prevent tmux from treating `send-keys` input as pasted text, which would require a second Enter to confirm. This makes prompt submission reliable without workarounds.

7. **Raw keystroke API**: The `/keys` endpoint sends bytes directly to the PTY, enabling the UI to interact with Claude CLI's interactive prompts (numbered choices, Esc/Tab/ctrl shortcuts) without the server needing to understand the prompt format.

8. **Single HTML file**: All CSS and main application JS live in `index.html`. Only reusable modules (ConvLogView, InputHistory, API wrapper, ANSI parser) are in separate JS files.

9. **Multiple CLI programs**: The server supports starting different CLI programs (e.g., `claude` or `opencode`). The `/api/instances` POST endpoint accepts an optional `cli_type` parameter to select the program.

10. **OpenCode pane sizing**: When creating an opencode session from the web UI, the tmux pane size is automatically set to match the available UI real estate. The height is dynamically based on `div#output-wrapper` height minus a small bottom margin (to accommodate the last prompt input) divided by character height. The width has a max to keep the session nicely readable. `calculateTerminalHeightInRows()` and `calculateTerminalWidthInCols()` and sends them via the `height` and `width` parameters in the create instance request. The server then calls `tmux resize-window -t <session> -y <height> -x <width>` after starting the session.

11. **OpenCode scroll handling**: Unlike `claude` which prints to stdout and relies on tmux scrollback, `opencode` is a TUI that keeps everything in one visible pane and manages its own scroll state. To support scrolling in the web UI, the frontend captures `wheel` events on `#output-area` for opencode sessions, calculates the number of lines scrolled, translates them into ANSI Up (`\x1b[A`) and Down (`\x1b[B`) arrow key sequences, and sends them via the `/keys` endpoint. The keys are batched (up to 50 arrows at a time) and flushed at 20fps to prevent queue flooding.
