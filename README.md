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

### Host Codex login and corporate certificate trust

The default Compose file uses independent named volumes for credentials; it
does **not** inherit the host's Codex login or macOS Keychain certificate trust.
To import a host login and trust your organization's HTTPS inspection CA, add
the following to `docker-compose.local.yml`:

```yaml
services:
  agent-manager:
    environment:
      AGENT_MANAGER_CODEX_AUTH_SOURCE: /mnt/host-codex-auth.json
      AGENT_MANAGER_EXTRA_CA_CERTS: /mnt/host-ca.pem
    volumes:
      - type: bind
        source: ${HOME}/.codex/auth.json
        target: /mnt/host-codex-auth.json
        read_only: true
        bind:
          create_host_path: false
      - type: bind
        source: ./certs.local/host-ca.pem
        target: /mnt/host-ca.pem
        read_only: true
        bind:
          create_host_path: false
```

Export the approved corporate CA certificates from your host trust store as PEM
into `certs.local/host-ca.pem` (excluded from Git and image build contexts).
Use CA certificates from your administrator or trusted host store, not an
unverified certificate downloaded from the failing connection. Omit the CA
mount and environment setting when no additional CA is needed. The entrypoint
combines these certificates with the system bundle and configures Python,
curl, Node/Claude, and Codex to use it with certificate verification enabled.

The entrypoint copies host Codex credentials into the existing `codex-auth`
volume, with mode `0600`, on the first import and whenever the host file changes
at startup. Container token refreshes are preserved when the host source is
unchanged; nothing is written back to the host. This is a startup import, not
live credential synchronization. After signing in again on the host or changing
the CA bundle, recreate the service to refresh the file mounts:

```bash
docker compose -f docker-compose.yml -f docker-compose.local.yml up -d --force-recreate
```

Codex must use file credential storage inside the container (the default).
If the host login exists only in Keychain, create a file-backed login with
`codex -c 'cli_auth_credentials_store="file"' login` before enabling the mount.
See [OpenAI Docs: authentication](https://developers.openai.com/codex/auth/).
Host and container token refreshes are independent; if a provider invalidates
a shared refresh token, sign in again and recreate the container, or use a
separate container login instead.

After building the updated image, verify with:

```bash
docker compose -f docker-compose.yml -f docker-compose.local.yml exec agent-manager codex login status
docker compose -f docker-compose.yml -f docker-compose.local.yml exec agent-manager claude auth status
```

The CA environment is set by the entrypoint for the server and its children.
For a diagnostic CLI launched with `docker exec`, invoke the entrypoint too:

```bash
docker exec agent-manager python /usr/local/bin/agent-manager-entrypoint.py curl -I https://api.anthropic.com
```

An HTTP error such as 401 or 404 still demonstrates a verified TLS connection;
it does not by itself verify provider credentials.

## Local image customization

For dependencies that should exist in your own Agent Manager image but not be
committed to this repo, edit the git-ignored local Dockerfile overlay:

```bash
vim Dockerfile.local
./scripts/build-local-image.sh
```

The local Dockerfile builds `FROM agent-manager:base`, so the standard image
remains reproducible and local changes stay isolated. A typical
`Dockerfile.local` can install system packages, Python packages from a
git-ignored `requirements.local.txt`, project CLIs, or other runtimes.

`./scripts/build-and-start.sh` and `./scripts/restart-agent-manager.sh`
initialize `Dockerfile.local` and `docker-compose.local.yml` from their example
files when they do not exist, then run Compose with the local overlay enabled.
The scripts build `agent-manager:base` first when the local overlay is enabled;
the generated `docker-compose.local.yml` then uses `Dockerfile.local` for the
running container image.

The default local Compose build block is:

```yaml
services:
  agent-manager:
    build:
      context: .
      dockerfile: Dockerfile.local
      args:
        BASE_IMAGE: agent-manager:base
    image: agent-manager:local
```

`Dockerfile.local`, `requirements.local.txt`, and
`docker-compose.local.yml` are git-ignored so each user can keep local runtime
customizations out of the shared Agent Manager image.

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
