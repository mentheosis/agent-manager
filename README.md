# agent-manager

Python web server for running and orchestrating multiple Claude Agent SDK sessions, each in its own working directory. Successor to [claude-squad](https://github.com/smtg-ai/claude-squad) (Go + tmux); uses the SDK's structured event stream instead of scraping a terminal buffer.

## Run

```bash
docker compose up --build
# → http://localhost:8787
```

On first launch, the container is not yet authenticated with Claude. A red banner in the UI invites you to click **Log in**; that runs `claude login` inside the container, streams the authorization URL to the browser, and lets you paste the returned code back. Credentials are written to a named docker volume (`claude-auth`) so they persist across rebuilds.

## How it works

The Python server wraps the official `claude-agent-sdk` Python package. That package spawns the `claude` CLI (from `@anthropic-ai/claude-code`) as a subprocess and pipes JSON in/out — **both** are installed in the container.

Each UI "instance" is one long-lived `ClaudeSDKClient` backed by one `claude` subprocess, running asynchronously inside the FastAPI server. The browser subscribes to a WebSocket and receives structured events (`assistant_text`, `tool_use`, `tool_result`, `thinking`, `result`, …) as the agent works.

## Filesystem & auth model

The container is **isolated** — no `$HOME` mount from the host. The only bind is a named volume for `/root/.claude` where the CLI stores its credentials file after `claude login`.

Implication: instances can only operate on paths **inside the container**. To work on a real host project, add an explicit bind mount in `docker-compose.yml`, e.g.:

```yaml
    volumes:
      - claude-auth:/root/.claude
      - ${HOME}/wrk/my-project:/work/my-project
```

and point the instance at `/work/my-project` in the UI. A project-mount UX is a later story.

## Status

v0: create instances, send prompts, stream structured events to browser, in-container Claude login via the UI. No persistence across server restarts, no git worktree isolation, no orchestrator yet.
