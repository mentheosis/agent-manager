# agent-manager

Python web server for running and orchestrating multiple Claude Agent SDK sessions, each in its own working directory. Successor to [claude-squad](https://github.com/smtg-ai/claude-squad) (Go + tmux); uses the SDK's structured event stream instead of scraping a terminal buffer.

## Run

```bash
docker compose up --build
# → http://localhost:8787
```

## Test

```bash
docker build --target python-test -t agent-manager:python-test .
docker build --target go-test -t agent-manager:go-test .
```

On first launch, the container is not yet authenticated with Claude. A red banner in the UI invites you to click **Log in**; that runs `claude auth login` inside the container, streams the authorization URL to the browser, and lets you paste the returned code back. Credentials are written to a named docker volume (`claude-auth`) so they persist across rebuilds.

## Working on host projects

The container is isolated by default — no host paths are mounted, so the agent can only see files that live inside the container's own filesystem. To let the agent work on a host git repo, copy the example override and add bind mounts:

```bash
cp docker-compose.local.yml.example docker-compose.local.yml
# edit docker-compose.local.yml to list the host directories you want exposed
```

Then run with both files:

```bash
docker compose -f docker-compose.yml -f docker-compose.local.yml up --build
```

Convention: **mount at the same absolute path** on both sides (e.g. `${HOME}/wrk/foo:${HOME}/wrk/foo`) so the path you paste into the UI's *Working directory* field works identically inside and outside the container. The container's `HOME` is `/app`, so `~` in the UI does *not* mean your host home.

For testing this repo from inside the container, mount it at `/app/agent-manager` and use that as the UI working directory:

```yaml
services:
  agent-manager:
    volumes:
      - ${HOME}/codes/agent-manager:/app/agent-manager
```

`docker-compose.local.yml` is git-ignored so each developer keeps their own.

## Local build dependencies

For dependencies that should be installed in your own Agent Manager image but not
committed to this repo, create a git-ignored `docker-build.local.sh` at the
repository root. The Dockerfile runs it as `root` during image build if it
exists.

Example:

```bash
#!/usr/bin/env bash
set -euo pipefail

pip install --no-cache-dir some-private-or-local-package

# System packages are also possible:
# apt-get update
# apt-get install -y --no-install-recommends graphviz
# rm -rf /var/lib/apt/lists/*
```

## How it works

The Python server wraps provider runtimes behind a common instance/event layer. Claude uses the official `claude-agent-sdk` Python package, which spawns the `claude` CLI from `@anthropic-ai/claude-code`. Codex uses `codex exec --json` from `@openai/codex`. Both CLIs are installed in the container.

Each UI "instance" runs asynchronously inside the FastAPI server and publishes provider-normalized events. The browser subscribes to a per-instance WebSocket and receives structured events (`assistant_text`, `tool_use`, `tool_result`, `thinking`, `result`, …) as the agent works. Per-instance WSs stay open in the background, so status indicators and conversation transcripts continue to update for non-selected conversations.

## Persistence

Three named volumes survive `docker compose down`:

- `claude-auth` → `/app/.claude` — Claude's credentials and per-session jsonl transcripts. The latter is what makes session resumption work.
- `codex-auth` → `/app/.codex` — Codex auth, config, and session storage used by `codex exec resume`.
- `agent-manager-state` → `/var/lib/agent-manager` — `instances.json` (registry: title, display_title, path, permission_mode, session_id, created_at, order) plus `events/{title}.jsonl` per instance (every UI event for replay).

On restart, persisted instances are re-created with their stored `session_id` passed as `resume=…` in `ClaudeAgentOptions`, so the agent picks up exactly where it left off.

## Status

v0: create instances, send prompts, stream structured events to browser, in-container Claude login via the UI, persistence + session resumption across restarts, drag-to-reorder, rename, optional host bind mounts. No git worktree isolation, no pause/resume, no orchestrator (leader/worker) yet.
