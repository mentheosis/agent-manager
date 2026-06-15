# Porting Session Management from tmux to the SDK / Headless-CLI Backend

Design + mapping document. Goal: replicate the tmux-based session-management
functionality (context monitoring → soft/hard/manual split → checkpoint handoff →
respawn) on a non-TUI "SDK" backend, as close to 1:1 as the SDK allows, and call
out where it cannot be 1:1.

Status: **design only** — nothing here is implemented yet.

---

## 1. The key decision: which "SDK"?

There are three candidates; they are NOT interchangeable.

| Option | What it is | Fit for this repo |
|---|---|---|
| **Headless CLI streaming-JSON** | `claude -p --output-format stream-json --input-format stream-json` — the same local agent harness we run today, but driven over stdin/stdout JSON instead of a TUI in a PTY. | **Best fit.** This repo is Go; this is a child process + pipes, no new runtime. It is also the transport the TS/Python SDK wraps. |
| **Claude Agent SDK (library)** | `@anthropic-ai/claude-agent-sdk` (TS) / `claude-agent-sdk` (Python). In-process `query()` + hooks + `interrupt()`. | No **Go** SDK exists. Using it means a Node/Python **sidecar** the Go server controls over IPC. More moving parts; only worth it if we need hooks (e.g. `PreCompact`). |
| **Server-side Sessions API** (`client.beta.sessions…`) | Managed agent sessions on Anthropic servers over HTTP. | Wrong layer — it does not run our local agent loop / local tools / local filesystem. Out of scope. |

**Recommendation: target the headless CLI streaming-JSON mode directly from Go.**
It keeps the "we drive a local `claude` process" architecture, removes tmux/PTY,
and gives us structured events instead of screen-scraping. The library/sidecar is
a fallback only if we later need the `PreCompact` hook (see Limitations §6).

---

## 2. Process & event model: today vs. headless

**Today (tmux):** `claude` runs as an interactive TUI inside a tmux pane (a PTY).
We *drive* it by typing keystrokes (`send-keys`, raw PTY bytes) and *observe* it by
screen-scraping (`capture-pane`, hashing the pane to detect change). Context usage
is recovered out-of-band by parsing the transcript JSONL.

**Headless:** `claude` runs as a child process with **stdin/stdout pipes**. We drive
it by writing newline-delimited JSON user messages to stdin, and observe it by
reading newline-delimited JSON events from stdout:

- `{"type":"system","subtype":"init","session_id":"<uuid>", …}` — first line, carries the session id.
- `{"type":"stream_event", …}` — token deltas (with `--include-partial-messages`).
- `{"type":"assistant","message":{"content":[…],"usage":{…}}, …}` — a response, with token usage.
- `{"type":"result","subtype":"success","session_id":…,"num_turns":N,"usage":{…},"total_cost_usd":…}` — **turn complete.**

This flips the two hardest tmux problems (idle detection and context measurement)
from heuristics into exact, structured signals.

---

## 3. Operation-by-operation mapping

Classification matches the tmux inventory: **CONTROL** (drive the agent),
**CONTEXT-READ** (measure pressure), **DISPLAY** (render to the web UI).

### 3a. CONTROL

| Capability | Today (tmux primitive) | Headless / SDK equivalent | 1:1? |
|---|---|---|---|
| Launch agent | `tmux new-session … claude --session-id <uuid>` (`tmux.go:99`, `instance.go:347`) | `exec` child: `claude -p --output-format stream-json --input-format stream-json --verbose --session-id <uuid>` with stdin/stdout pipes | **Yes** |
| Pin session id | `--session-id <uuid>` launch flag | identical `--session-id <uuid>` | **Yes** (unchanged) |
| Send prompt (wrap-up / restore / steer) | `send-keys -l <text>` + `Enter` (`tmux.go:258-270`) | write one stdin line: `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}` | **Yes** |
| Idle / "ready" detection | `capture-pane` + hash-diff (`HasUpdated`, `tmux.go:294-317`); status set in `pollMetadata` (`server.go:227-243`) | a `result` message means the turn finished → idle; process awaiting next stdin line = idle. Event-driven, no polling/hashing. | **Yes — and strictly better** |
| Interrupt (hard split) | raw PTY write `0x1b` Escape (`tmux.go:248`, `SendInterrupt` `manager.go:138`) | streaming-input control request `{"type":"control_request","request":{"subtype":"interrupt"}}` on stdin, **or** SIGINT to the child. (SDK exposes `query.interrupt()` over this same channel.) | **Likely — must validate** (see §8) |
| Respawn / split (reset context) | `tmux respawn-pane -k <program>` with a freshly minted `--session-id` (`instance.go:773-788`, `tmux.go:274`) | kill the current child, spawn a new `claude` child with a **new** `--session-id`, feed the restore prompt as its first stdin message | **Yes** (cleaner — plain process restart, no pane) |
| `/clear` button | type `/clear` into pane (just added) | start a fresh session (= a respawn with a new id, no checkpoint) | **Equivalent** |
| `/compact` button | type `/compact` into pane (just added) | **no programmatic trigger** in headless; auto-compaction only (observable via SDK `PreCompact` hook, not raw CLI) | **No** (see §6) |
| Trust-folder prompt dismiss | scrape "Do you trust this folder?" + Enter (`tmux.go:161-182`) | not a TUI in headless; use `--permission-mode` (e.g. `acceptEdits`/`plan`) or `--dangerously-skip-permissions`, and a permission callback for tool gating | **N/A** (config, not scraping) |
| Close / kill | `tmux kill-session` (`tmux.go:484`) | close stdin / kill child process | **Yes** |

### 3b. CONTEXT-READ (context pressure)

| Capability | Today | Headless / SDK | 1:1? |
|---|---|---|---|
| Per-turn token usage | parse tail of `~/.claude/projects/<mangled>/<id>.jsonl` (`context_reader.go`) | read `usage` directly off each `assistant`/`result` event (`input_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`, `output_tokens`) | **Yes — and exact** (no tail-parsing race; the JSONL still persists, so the existing reader keeps working as a fallback) |
| Context window size | auto-detect `[1m]` from `~/.claude/settings.json`, else 200K (with the loud-warning fix) | unchanged — still needed to turn tokens into a % | **Yes** (unchanged) |
| Checkpoint freshness wait | `os.Stat` mtime vs. baseline (`waitForFreshCheckpoint`) | unchanged — the agent still writes `checkpoint-N.md`; we still gate the respawn on a fresh file | **Yes** (unchanged) |

This whole layer is already tmux-agnostic. The only change is an *additional,
better* token source (the `usage` events) feeding `ContextMonitor.IngestContextUsage`.

### 3c. DISPLAY (web UI) — the least 1:1 area

| Capability | Today | Headless / SDK | 1:1? |
|---|---|---|---|
| Live agent view | `capture-pane` → `ConversationLog` → WebSocket → browser (`pollOutput`, `convlog.go`) | assemble a transcript view from `assistant` text + `stream_event` deltas + tool_use/tool_result events → WebSocket | **No — UI rework.** Same data path *shape* (ingest → WS), different ingest source. We render structured messages, not a raw terminal. |
| Scrollback / history size | `capture-pane -S/-E`, `display-message "#{history_size}"` | accumulate streamed messages in a buffer; "size" = message/line count we keep | Conceptual analog |
| Interactive-prompt detection | regex over pane text (`pane_parser.go`) | structured permission/control-request events instead of parsed pane text | Different model |
| User attaches a real terminal | `tmux attach` | **lost** — there is no pane to attach to | **No** (feature loss) |

---

## 4. Proposed architecture

The Explore pass confirmed the single leverage point: `session/tmux.TmuxSession` is a
**concrete struct with no interface**, and idle detection currently leaks tmux into
`pollMetadata` (it calls `inst.HasUpdated()` directly). So two changes unlock a backend swap.

### 4a. Introduce a `Backend` interface

Define the seam in `session/` and make `Instance` hold a `Backend` instead of a
`*tmux.TmuxSession`:

```go
// session/backend.go
type Backend interface {
    Start(workDir, program, sessionID string) error
    SendPrompt(text string) error          // wrap-up / restore / steer
    Interrupt() error                       // hard-split Escape equivalent
    Respawn(program, newSessionID string) error
    Status() Status                         // Running | Ready | NeedsInput (event-derived)
    Events() <-chan Event                   // assistant/result/usage/tool events
    Close() error
}
```

- `TmuxBackend` = today's `TmuxSession` (renamed; keep it working, default).
- `HeadlessBackend` = new: owns the `claude` child process, a stdin JSON writer,
  and a stdout JSON scanner goroutine that turns lines into `Event`s and updates
  `Status` (`result` → Ready, new turn → Running).

`ManagerCallbacks` barely changes — `SendPrompt`/`RespawnClaude`/`SendKeys`(→`Interrupt`)/`GetStatus`
already route through `Instance`; they just delegate to `Backend` instead of `TmuxSession`.

### 4b. Move idle detection behind the backend

`pollMetadata` must stop calling `HasUpdated()` (a tmux capture) for the split gate.
Instead, the backend reports `Status()`:
- tmux backend: keep the capture-pane/hash logic inside `TmuxBackend.Status()`.
- headless backend: derive from the event stream (`result` ⇒ Ready).

The soft/hard/manual split gate (`meta.Status == "ready"` + the `*Now()` checks +
cooldown, all the recent fixes) is then backend-agnostic and unchanged.

### 4c. Context feed

In headless mode, the stdout scanner calls `ContextMonitor.IngestContextUsage`
directly from the `usage` on each `assistant`/`result` event — `pollContextUsage`'s
JSONL reader becomes a fallback (still valid; transcripts persist). Soft/hard
thresholds, live re-validation, and cooldown are untouched.

### 4d. Handoff = process restart

`executeHandoff` is already backend-neutral except for the two callbacks it uses
(`sendPrompt`, `respawnClaude`). Mapping:
1. wrap-up prompt → `Backend.SendPrompt` (stdin user message).
2. wait for fresh `checkpoint-N.md` → unchanged.
3. respawn → `Backend.Respawn` (headless: kill child, spawn new child with new id).
4. restore prompt → `Backend.SendPrompt` to the new child.

All of §3a's recent correctness work (arm-and-wait manual split, live-revalidated
thresholds, cooldown, checkpoint-mtime gate, generous timeout) carries over verbatim
because it lives above the backend seam.

---

## 5. What ports cleanly (and improves)

- **Idle detection** — exact (`result` event) instead of pane-hash heuristics.
- **Context measurement** — exact per-turn `usage` instead of JSONL tail-parsing; removes the window/200K guessing risk for the *trigger* (still need window for the %).
- **Send prompt / interrupt / respawn / pinned session id / checkpoint gate** — direct equivalents.
- **The entire split state machine + all recent bug fixes** — unchanged (above the seam).

## 6. Limitations / NOT 1:1

1. **`/compact` cannot be triggered programmatically** in headless mode. Auto-compaction still happens; it is only *observable* (and only via the SDK library's `PreCompact` hook, not the raw CLI). → the **Compact button** does not port to a pure headless backend. Options: (a) drop it in SDK mode, (b) run the library sidecar just to get `PreCompact`, (c) rely on our own split (which supersedes `/compact` anyway — a split resets context harder than a compaction).
2. **Live terminal view / `tmux attach`** is lost — there is no PTY to attach to. The web UI must render a transcript from JSON events (real work; see §3c). Users who attach a real terminal to the pane lose that.
3. **Interactive permission prompts** change model: from pane scraping (`pane_parser.go`) to structured permission/control events + `--permission-mode`. Net simpler, but it is a rewrite of that path.
4. **Interrupt semantics need validation** — whether the stdin `control_request` interrupt works in raw headless mode, or whether we fall back to SIGINT (and whether SIGINT loses the turn vs. pausing cleanly). See §8.
5. **Multi-turn within one process** relies on `--input-format stream-json` keeping the child alive across turns. The SDK does this; the exact stdin message schema for the raw CLI was **not fully verifiable from docs** and must be confirmed by a spike (§8).
6. **No Go SDK** — anything needing the *library* (hooks) requires a Node/Python sidecar.

## 7. Phased implementation plan

1. **Spike (½–1 day):** drive `claude` headless from a throwaway Go script: spawn with `--session-id`, `--input-format stream-json --output-format stream-json --verbose`; confirm multi-turn stdin schema, the `result`-as-idle signal, `usage` fields, interrupt mechanism, and that `checkpoint-N.md` + transcript still get written. Resolve every §8 question before building.
2. **Backend interface (§4a):** extract `Backend`, rename current code to `TmuxBackend`, make `Instance` hold a `Backend`. Pure refactor; tmux still default; all tests green.
3. **Move idle detection behind `Status()` (§4b)** so `pollMetadata` no longer screen-scrapes for the gate. Still tmux underneath.
4. **`HeadlessBackend` (§4a):** child-process manager + stdin writer + stdout JSON scanner → `Status`/`Events`; wire `SendPrompt`/`Interrupt`/`Respawn`/usage-ingest.
5. **DISPLAY (§3c):** new convlog ingest that builds a transcript from JSON events; WebSocket shape unchanged.
6. **Permissions (§3, item):** replace trust/permission pane-scraping with `--permission-mode` + a permission event handler.
7. **Per-instance backend toggle** (tmux vs headless), default tmux until headless is proven.

## 8. Spikes / open questions to resolve first

- Exact stdin schema for streaming user messages in raw CLI `--input-format stream-json`, and whether the process stays alive for multiple turns.
- Interrupt: does a stdin `control_request{interrupt}` work in raw headless, or is SIGINT the only option? Does it pause cleanly (so we can then send the wrap-up) or abort the turn?
- Does `--print` headless persist `checkpoint-N.md` writes and the transcript JSONL exactly as the TUI does (it should — same harness — but confirm)?
- Permission model in headless: can the agent run our wrap-up/checkpoint writes without interactive approval under a chosen `--permission-mode`?
- Does a fresh `--session-id` child start at ~0 context (true reset) as expected for the split?

---

## TL;DR

Target the **headless CLI streaming-JSON** transport from Go. The **control** and
**context-read** halves port cleanly — often *better* (exact idle + token signals).
The **display** half (live terminal view) and the **`/compact`** trigger are the real
non-1:1 losses. The clean seam is a `Backend` interface in `session/` plus moving idle
detection out of `pollMetadata`; everything above that seam — the whole split/handoff
state machine and all the recent fixes — is reused unchanged.
