# Agent Manager Architecture

Agent Manager is a web server for running and orchestrating multiple LLM coding agent sessions. Each session runs in its own working directory with its own conversation history, while sharing a common event-driven UI.

## Core Design Principles

1. **Provider Abstraction** — Different LLM backends (Claude, Codex, future providers) implement a common `AgentRuntime` interface, normalizing their outputs into a unified event stream.

2. **Container Isolation** — The agent runs inside Docker with no default host access. Host directories are explicitly bind-mounted; host commands run through a separate, locked-down MCP server.

3. **Persistence & Resume** — Session IDs are stored so conversations survive container restarts. The provider's own session storage (Claude's `.claude/projects/` jsonl, Codex's `.codex/sessions/`) handles transcript continuity.

4. **Separation of Concerns** — Three independent components (core server, Docker MCP, orchestrator) run as separate processes with distinct security boundaries.

## Components

### 1. Agent Manager Server (Python/FastAPI)

The core server manages instances and streams events to the browser.

```
┌─────────────────────────────────────────────────────────┐
│                  Agent Manager Server                    │
│                                                          │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐               │
│  │ Instance │  │ Instance │  │ Instance │  ...          │
│  │ (Claude) │  │ (Codex)  │  │ (Loop)   │               │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘               │
│       │             │             │                      │
│       ▼             ▼             ▼                      │
│  ┌─────────────────────────────────────┐                │
│  │         Provider Registry           │                │
│  │  ClaudeRuntime  │  CodexRuntime     │                │
│  └─────────────────────────────────────┘                │
│                      │                                   │
│                      ▼                                   │
│  ┌─────────────────────────────────────┐                │
│  │      Normalized Event Stream        │                │
│  │  assistant_text, tool_use,          │                │
│  │  tool_result, thinking, result, ... │                │
│  └─────────────────────────────────────┘                │
│                      │                                   │
│                      ▼                                   │
│  ┌──────────────┐  ┌─────────────────┐                  │
│  │  WebSocket   │  │   Persistence   │                  │
│  │  (per inst)  │  │ instances.json  │                  │
│  └──────────────┘  │ events/*.jsonl  │                  │
│                    └─────────────────┘                  │
└─────────────────────────────────────────────────────────┘
```

**Key abstractions:**

- **Instance** — One conversation session. Has a title, working directory, provider, permission mode, model, and session ID for resume.
- **AgentRuntime** — Protocol for provider backends. Implements `start()`, `run_turn(message)`, `close()`.
- **Registry** — Manages instance lifecycle, persistence, and state changes.

**Provider normalization:**

| Provider | CLI | Events From |
|----------|-----|-------------|
| Claude | `claude` (via SDK) | `AssistantMessage`, `UserMessage`, `ResultMessage`, `SystemMessage` |
| Codex | `codex exec --json` | JSONL stdout + transcript file tail |

Both are translated into common event types: `assistant_text`, `tool_use`, `tool_result`, `thinking`, `result`, `error`, `status`.

### 2. Docker MCP Server (Go)

Runs **on the host**, exposes pre-approved Docker commands to agents inside the container.

```
┌────────── HOST ──────────┐        ┌──── CONTAINER ────┐
│                          │        │                   │
│  am-docker-mcp           │◀─HTTP──│  Claude Agent     │
│   ├─ config.json         │        │  mcp__docker__*   │
│   └─ exec("docker ...")  │        │                   │
└──────────────────────────┘        └───────────────────┘
```

**Why separate?**

- **Security boundary** — The container cannot run arbitrary host commands. Only profiles defined in `config.json` are exposed.
- **No shell injection** — Commands are `exec()`ed directly, not passed through a shell.
- **Async jobs** — Long-running builds return a `job_id`; the agent polls for status/logs.

**Exposed tools:**
- `start_job(profile)` — Start a named job
- `get_job_status(job_id)` — Check if running/done/failed
- `tail_job_log(job_id, lines)` — Get recent output
- `list_profiles()` — Show available commands

### 3. Orchestrator (Go)

Coordinates multiple agents working on a shared task (experimental).

```
┌─────────────────────────────────────────────────────────┐
│                    Loop Instance                         │
│                                                          │
│  ┌─────────────────┐                                    │
│  │  Orchestrator   │  (Go binary: am-orchestrator)      │
│  │  "Leader"       │                                    │
│  └────────┬────────┘                                    │
│           │ polls status, dispatches tasks              │
│           ▼                                             │
│  ┌────────────────────────────────────┐                 │
│  │         Child Agents               │                 │
│  │  ┌──────┐  ┌──────┐  ┌──────┐     │                 │
│  │  │Coder │  │Tester│  │Rsrch │ ... │                 │
│  │  └──────┘  └──────┘  └──────┘     │                 │
│  └────────────────────────────────────┘                 │
└─────────────────────────────────────────────────────────┘
```

**Why separate?**

- **Different execution model** — The orchestrator runs a control loop (watch → batch → dispatch), not a request/response cycle.
- **MCP interface** — Provides tools for the leader agent: `list_agents`, `send_message`, `get_status`, `mark_task_done`.
- **Crash isolation** — If the orchestrator crashes, child agents and the main server continue running.

## Memory & Context

### Current State

1. **Static memory file** — User specifies a file path in Settings; contents are prepended to every prompt with:
   > "Here is all the important context that you must remember when considering my input below:"

2. **Artifact instructions** — Protocol for showing files in the UI is also prepended to every prompt.

3. **Provider-side context** — Claude/Codex maintain their own conversation history. When context fills, the provider may auto-compact.

### Persistence Layout

```
/var/lib/agent-manager/
├── instances.json          # Registry: all instance metadata
└── events/
    ├── my_project.jsonl    # UI event stream for replay
    └── another.jsonl

~/.claude/
├── projects/               # Claude's session transcripts (via SDK)
└── backups/

~/.codex/
└── sessions/               # Codex session transcripts
```

## Data Flow

```
User prompt
    │
    ▼
┌──────────────────────┐
│  _prompt_with_context │  ← Prepend: memory_file + artifact_instruction
└──────────────────────┘
    │
    ▼
┌──────────────────────┐
│  Provider Runtime     │  ← Claude SDK or Codex CLI
└──────────────────────┘
    │
    ▼
┌──────────────────────┐
│  Event Translation    │  ← Normalize to common types
└──────────────────────┘
    │
    ├──▶ WebSocket (UI)
    ├──▶ Persistence (events/*.jsonl)
    └──▶ Instance._history (in-memory, capped at 2000)
```

## Why This Architecture?

| Decision | Rationale |
|----------|-----------|
| Provider abstraction | Add new LLM backends without changing core logic |
| Separate MCP server | Host commands need host privileges; container isolation preserved |
| Separate orchestrator | Control loop semantics differ from request/response; crash isolation |
| Event normalization | UI code doesn't care which provider generated the event |
| Session ID resume | Container restarts don't lose conversation history |
| Memory file prepend | Persistent context survives compaction; user controls what's "important" |

## Future Directions

- **Semantic memory** — Extract facts from conversations into a vector database; retrieve relevant context per-prompt instead of static file prepend
- **Tool result filtering** — Strip noisy build output before it consumes context
- **Manual compaction trigger** — Expose `/compact` command through UI
- **Cross-agent memory** — Share learnings between instances working on the same codebase

## Development Rules

### Never Hack Patches Into the Running Environment

When making code changes to Agent Manager:

- **DO NOT** copy files directly into the running container's served paths (e.g. `/app/static/`, installed Python package paths)
- **DO NOT** edit files inside the running container outside of the bind-mounted source tree
- **DO NOT** apply "hot patches" to work around stale builds

Instead:

1. Make all edits to the source tree (`/app/agent_manager/` which is bind-mounted from the host)
2. Ask the user to restart the environment using `scripts/restart-agent-manager.sh`
3. This rebuilds the image cleanly, ensuring static files, installed packages, and code are all consistent

**Rationale:** Hot-patched files create divergence between the running env and the source of truth. The next legitimate rebuild may silently revert your fix or produce inconsistent behavior. A clean rebuild via the script guarantees the running env matches the committed source.
