# docker-mcp

A small MCP server that runs **on the host** and exposes a fixed set of pre-approved commands (typically `docker build` / `docker compose`) to a Claude Agent SDK client running **inside a container**.

The server is the security boundary. Claude can only invoke commands the operator has explicitly listed in a JSON config; it cannot inject arguments, change the working directory, or run anything else. There is no `run_command(string)` tool.

## Architecture

```
┌────────── host ──────────┐         ┌──── agent-manager container ────┐
│                          │         │                                 │
│  am-docker-mcp daemon    │ ◀──HTTP─│  Claude (Agent SDK)             │
│   ├─ config.json         │         │  → mcp__docker__start_job(...)  │
│   └─ exec("docker ...")  │         │                                 │
└──────────────────────────┘         └─────────────────────────────────┘
```

* **Host process**: a single Go binary (`am-docker-mcp`) reads a JSON config of named *profiles* and listens for MCP JSON-RPC requests over HTTP.
* **Auth**: every request must present a bearer token sourced from an env var (default `DOCKER_MCP_TOKEN`). Without a token, the daemon refuses to start unless `--allow-no-auth` is passed.
* **Wire**: container reaches the host on `host.docker.internal:9090` (works out of the box on Docker Desktop; needs `extra_hosts` on Linux — see below).
* **Output**: long-running commands return a `job_id` immediately; Claude polls `tail_job_log` and `get_job_status` until done. Logs are persisted on disk and a recent-line ring is kept in memory for cheap tailing.

## Security model

1. **No string-to-shell**. Each profile's `argv` is passed straight to `exec.Command(argv[0], argv[1:]...)`. No shell, no globbing, no env interpolation.
2. **No client-supplied arguments**. The `start_job` tool takes only `profile: string`. The argv comes entirely from the on-disk config.
3. **Working directory pinned per profile**. The config validates that `cwd` exists and is a directory at startup.
4. **Minimal child env**. The host environment is *not* inherited by default. Only `PATH`, `HOME`, `USER`, `LOGNAME`, and the docker-relevant vars (`DOCKER_HOST`, `DOCKER_CONFIG`, `DOCKER_CONTEXT`, `DOCKER_BUILDKIT`, `BUILDX_CONFIG`) are passed through. Add per-profile `env` entries for anything else.
5. **Bearer token**. HTTP server requires `Authorization: Bearer <token>` on every endpoint except `/healthz`. Token is read from an env var so it never lives on disk in cleartext.
6. **Loopback by default**. `listen` defaults to `127.0.0.1:9090`. Bind to a non-loopback address only deliberately.
7. **Per-profile concurrency cap**. Defaults to 1 — a profile cannot have more than one in-flight job at a time. Set `max_concurrent_per_profile: 0` to disable.
8. **Per-job timeout**. Each profile sets `timeout_seconds`. On timeout the entire process group (Unix) gets SIGTERM, then SIGKILL.

## Build

Independent of agent-manager's container build. From the repo root:

```bash
cd docker-mcp
go build -o am-docker-mcp .
# binary lands at ./am-docker-mcp; copy wherever you keep host binaries
```

No external Go dependencies — stdlib only.

## Configure

The config file lives alongside the binary in the repo:

```bash
cd agent_manager/docker-mcp
cp docker-mcp.example.json config.json
$EDITOR config.json
```

Each profile is a fully-fixed command. You can specify the command as either an `argv` array or a `script` path:

**Using argv (explicit command vector):**
```json
{
  "name": "build-agent-manager",
  "description": "Build the agent-manager image.",
  "cwd": "/Users/me/wrk/agent_manager",
  "argv": ["docker", "compose", "build"],
  "timeout_seconds": 1800
}
```

**Using script (reference a .sh file):**
```json
{
  "name": "build-agent-manager",
  "description": "Build the agent-manager image.",
  "cwd": "/Users/me/wrk/agent_manager",
  "script": "./scripts/build.sh",
  "timeout_seconds": 1800
}
```

The `script` path can be absolute or relative to `cwd`. By default it's run via `bash <script>`.

**Using a login shell (to get your ~/.zshrc environment):**

Add a `shell` option to your config to use a login shell that sources your profile:

```json
{
  "shell": ["zsh", "-l"],
  "profiles": [...]
}
```

This runs scripts as `zsh -l <script>`, which sources `~/.zshrc` and gives you access to your normal environment variables, PATH additions, etc.

Verify the config parses cleanly:

```bash
./am-docker-mcp -print-config
```

## Run

```bash
export DOCKER_MCP_TOKEN=$(openssl rand -hex 32)
./am-docker-mcp
```

Or use the helper script from the repo root:

```bash
./hostmcp-build-and-start.sh
```

Health check (no auth required):

```bash
curl -s http://127.0.0.1:9090/healthz
```

Smoke-test from the host (auth required):

```bash
curl -s http://127.0.0.1:9090/ \
  -H "Authorization: Bearer $DOCKER_MCP_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | jq .
```

For local development you can also run in stdio mode (one JSON object per line):

```bash
./am-docker-mcp -stdio
```

## Wire up the agent-manager container

### macOS / Windows (Docker Desktop)

`host.docker.internal` is resolvable inside the container by default. Just pass the token in to the agent-manager container so Claude can use it:

```yaml
# docker-compose.local.yml
services:
  agent-manager:
    environment:
      DOCKER_MCP_URL: "http://host.docker.internal:9090"
      DOCKER_MCP_TOKEN: "${DOCKER_MCP_TOKEN}"
```

Then `DOCKER_MCP_TOKEN=… docker compose -f docker-compose.yml -f docker-compose.local.yml up`.

### Linux

Add `extra_hosts` so `host.docker.internal` resolves inside the container, and either:

* bind `am-docker-mcp` to `0.0.0.0:9090` (firewall to localhost), **or**
* bind to the docker bridge IP (typically `172.17.0.1:9090`).

```yaml
# docker-compose.local.yml
services:
  agent-manager:
    extra_hosts:
      - "host.docker.internal:host-gateway"
    environment:
      DOCKER_MCP_URL: "http://host.docker.internal:9090"
      DOCKER_MCP_TOKEN: "${DOCKER_MCP_TOKEN}"
```

The bearer token is the only thing standing between any process on the host network and your build commands. Treat it like an SSH key.

## Register with the Claude Agent SDK

In agent-manager's Python code (where it constructs `ClaudeAgentOptions`), add a streamable-HTTP MCP server:

```python
options = ClaudeAgentOptions(
    mcp_servers={
        "docker": {
            "type": "http",
            "url": os.environ["DOCKER_MCP_URL"] + "/",
            "headers": {
                "Authorization": f"Bearer {os.environ['DOCKER_MCP_TOKEN']}",
            },
        },
    },
    # Optional: pre-allow these so the user isn't prompted every time.
    allowed_tools=[
        "mcp__docker__list_profiles",
        "mcp__docker__start_job",
        "mcp__docker__get_job_status",
        "mcp__docker__tail_job_log",
        "mcp__docker__cancel_job",
        "mcp__docker__list_jobs",
    ],
    ...
)
```

The exact field names depend on which version of the SDK you're on; consult the SDK's MCP-server docs if the keys above don't match.

## Tools

| Tool | Args | Returns |
| --- | --- | --- |
| `list_profiles` | – | `{profiles: [...], count}` |
| `start_job` | `{profile}` | job snapshot incl. `id`, `state`, `started_at` |
| `get_job_status` | `{job_id}` | full job snapshot |
| `tail_job_log` | `{job_id, since_line?, max_lines?}` | `{lines, line_count, next_cursor, done, state}` |
| `cancel_job` | `{job_id}` | text confirmation |
| `list_jobs` | – | `{jobs: [...], count}` (newest first) |

### Typical Claude flow for "build the image and fix any errors"

1. `list_profiles` — discover what's available.
2. `start_job(profile="build-agent-manager")` — get a `job_id`, build runs in background.
3. Loop: `tail_job_log(job_id, since_line=cursor)` until `done=true`, accumulate output.
4. `get_job_status(job_id)` — confirm exit code.
5. If non-zero exit, grep the tailed output for the error, edit source, repeat from (2).

## Limitations / TODO

* **No log truncation**. Logs grow until the daemon restarts. Add a sweeper if you run many jobs per session.
* **In-memory job registry**. Job metadata is lost on daemon restart (logs persist on disk). Fine for dev workflow.
* **Single-tenant token**. There is one bearer token, not per-client tokens. Rotate by restarting the daemon with a new env value.
* **No streaming progress**. Polling-only, by design. If you ever want live streaming, MCP supports progress notifications inside a single tool call — a future addition.
