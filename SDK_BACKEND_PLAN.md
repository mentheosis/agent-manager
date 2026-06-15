# Porting Session Management to the SDK app (`kw-rewrite-for-sdk`)

This branch (`sdk-session-management`, off `kw-rewrite-for-sdk`) ports the session-
management feature — context-pressure-driven **split / checkpoint / handoff** — from
the Go/tmux app on `main` into this **Python app built on `claude-agent-sdk`**.

The Go source of truth lives on the `session-management` branch (`sessionmgr/*.go`).
This document maps each piece onto this codebase's actual primitives.

---

## 1. Why this is cleaner than the tmux version

The Go version drove a TUI in a tmux pane and *scraped* it. This app runs each agent
as a `ClaudeSDKClient` and is **event-driven** (`src/agent_manager/instance.py`). The
three hardest tmux problems are already solved here:

| Need | tmux (Go) | This app (Python SDK) |
|---|---|---|
| Idle / "ready" detection | hash-diff `capture-pane` | `Instance.status` flips `running`→`ready` after `receive_response()` completes (`instance.py:138`) |
| Token / context usage | parse transcript JSONL | `ResultMessage.usage` → emitted as the `result` event (`instance.py:350-362`) |
| Session identity / resume | `--session-id` + respawn-pane | `session_id` captured in `_publish` (`instance.py:266-270`), replayed as `resume=` on (re)start (`instance.py:70-72`) |
| Send prompt | `send-keys` | `Instance.send(text)` (`instance.py:146`) |
| Interrupt | Escape byte to PTY | `Instance.abort()` (cancel + restart, conversation preserved) (`instance.py:207-223`) |
| Respawn / restart | `respawn-pane -k` | `Instance.reload_options()` / task restart (`instance.py:225-242`) |
| Permissions | scrape trust prompt | `permission_mode` option (`instance.py:37,68`) |

So we do **not** poll. The session manager reacts to the event stream the Instance
already publishes (every event flows through `_publish` → `_on_event` hook and
`subscribe()` queues).

## 2. The split, expressed in SDK terms

A "split" = reset the agent's context while preserving execution state via a checkpoint
file. In this app that is:

1. **Arm** (manual button) or **auto-arm** (threshold crossed): set a pending flag.
2. **Fire when idle** (`status == "ready"`): `Instance.send(wrap_up_prompt)`.
3. **Wait for a fresh `checkpoint-N.md`** on disk (unchanged from Go — file + mtime baseline).
4. **Reset context**: restart the SDK client **without** `resume` and with a **fresh
   session** (clear `session_id` so no `resume=` is passed, mint/let the SDK assign a new
   one) — this is the key difference from `reload_options()`, which *keeps* the conversation.
5. **Restore**: `Instance.send(restore_prompt)` pointing the fresh session at the checkpoint.

A **hard split** additionally calls `Instance.abort()` first to settle a busy agent, then
proceeds. `/clear` ≈ step 4 with no checkpoint. `/compact` has **no SDK trigger** (same
limitation as before; our split supersedes it).

## 3. Module plan (new files in `src/agent_manager/`)

- **`session_manager.py`** (start here — pure logic, no SDK import):
  - `SessionConfig` — enabled, soft/hard pct, cooldown, checkpoint timeout, window size. Port of `config.go`.
  - `ContextMonitor` — ingest `usage` → pct; latched soft/hard flags; **live-revalidated** `should_soft_split_now()` / `should_hard_split_now()`; **post-split cooldown**; **manual-split arm**. Direct port of `monitor.go` (incl. all the bug fixes: live re-validation, cooldown, arm-and-wait).
  - `SessionState` — current_session, checkpoints list. Port of the Go state.
- **`checkpoint.py`** — checkpoint paths under `~/.claude-squad/sessions/<title>/checkpoint-N.md` (or a state-dir equivalent), wrap-up / hard-wrap-up / restore prompt templates, mtime-baseline freshness. Port of `checkpoint.go`.
- **`session_handoff.py`** (or fold into `state.Registry`) — the async handoff state machine: arm → fire-on-idle → send wrap-up → await fresh checkpoint (generous timeout) → restart fresh (no resume) → send restore. Port of `manager.go executeHandoff`, but `async` and event-driven instead of goroutine + polling.

## 4. Wiring into the existing app

- **`Instance` (`instance.py`)**: add an optional `ContextMonitor`. In `_publish` (or via the
  `_on_event` hook), feed `result.usage` to the monitor and, on `status == "ready"`, ask the
  handoff coordinator whether a split should fire. Add a fresh-restart path (restart `_run`
  with `session_id=None`) for step 4 of §2. Expose `request_manual_split()`.
- **`Registry` (`state.py`)**: owns the handoff coordinator; wires the monitor when an instance
  is created/loaded; persists `SessionConfig`/`SessionState`.
- **`persistence.py` `InstanceRecord`**: add `session_config` + `session_state` fields
  (mirrors how `main` stores them in state.json). Backward-compatible defaults.
- **`server.py` (FastAPI)**: add `GET /api/instances/{title}/session` (config + state + pressure),
  `PUT .../session/config`, `POST .../session/split` (arm manual split). Mirrors the Go endpoints.
- **UI (`static/`)**: a Sessions tab — enable toggle, soft/hard %, pressure bar, checkpoint list,
  Split + Clear/Compact buttons. (Separate from the Go web UI; ported against this app's static frontend.)

## 5. Context window size

The SDK reports `usage` (tokens) but not the model's window. Keep a small model→window map
(and honor a `[1m]` suffix), defaulting conservatively with a loud log on fallback — same lesson
as the Go side (a silent 200K default makes thresholds trip ~5× too early on a 1M model). The
`model` is available from the `system_init` event (`instance.py:274-277`).

## 6. Limitations (carried over / new)

- **No programmatic `/compact`** — auto-compaction only (observable via the SDK `PreCompact`
  hook if we want to log it). Our split is the real mechanism.
- **No live terminal view** to port — this app already renders structured events, so DISPLAY is
  not a concern here (unlike the Go→headless gap).
- **Interrupt** is `abort()` (cancel + restart) rather than a mid-turn pause; confirm it settles
  cleanly before sending the wrap-up.

## 7. Increments (each independently testable)

1. **`session_manager.py`** (`SessionConfig` + `ContextMonitor`, pure logic) + unit tests — **port of the most-tested Go code; no SDK needed.** ← starting here.
2. `checkpoint.py` (paths, prompts, mtime freshness) + tests.
3. `session_handoff.py` async state machine + tests (fake Instance).
4. Wire into `Instance` / `Registry` / `persistence` (event-driven feed + fire-on-idle).
5. FastAPI endpoints.
6. Sessions-tab UI.
7. Per-instance enable toggle; default off.

---

## TL;DR

The SDK app already provides idle, usage, resume, send, interrupt, and permissions as
first-class primitives, so the port is mostly **lifting the pure split/threshold logic**
(`config.go` + `monitor.go`, with all the recent fixes) into Python and re-expressing the
handoff as an `async`, event-driven coordinator instead of a polling goroutine. Start with
`session_manager.py` (pure logic + tests), then layer the handoff, wiring, endpoints, and UI.
