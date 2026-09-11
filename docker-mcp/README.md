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

## Typed Athena queries (macOS/Linux host)

Athena profiles run `athena_runner.py` using a fixed absolute Python executable,
with isolated Python imports (`-I`) and JSON stdin. SQL is never interpolated into
argv or a shell. Existing command profiles remain supported in the same instance.
`start_job` rejects Athena profiles; use `athena_query` instead.

Host setup:

```bash
python3 -m venv /absolute/host/path/athena-venv
/absolute/host/path/athena-venv/bin/pip install -r requirements-athena.txt
aws sso login --profile YOUR_SSO_PROFILE
aws sts get-caller-identity --profile YOUR_QUERY_PROFILE
go build -o am-docker-mcp .
```

Copy the object in `athena-profile.example.json` into your existing config's
`profiles` array. Replace absolute paths and AWS profile, verify workgroup/results
location, and explicitly enumerate all permitted tables as `catalog.database.table`.
Restart the daemon with that config. No active host config is modified by this change.
The workgroup may override the configured results location. The host query profile
must resolve the intended SSO/assumed role; AWS credentials are not passed from the
container. Full environment inheritance is disabled for Athena jobs even when the
daemon uses `inherit_env: true` for other profiles.

Call `athena_query` with:

```json
{"profile":"prod-audit","sql":"SELECT kind, sum(rewards) FROM transactions_cumulatives WHERE symbol = 'LPT' GROUP BY kind","max_rows":1000}
```

Returns the existing job snapshot immediately. Poll `get_job_status`, then read
`tail_job_log` starting at `since_line: 1`. Output is JSON lines: query execution ID,
then column metadata, rows, truncation flag and Athena execution statistics (or an
error and nonzero job exit). Values remain strings, with SQL NULL represented as
JSON null. This version returns a bounded preview, not a downloadable full export.
Use narrower queries if truncated. Existing log retention behavior is unchanged.

Enforced limits: HTTP body 128 KiB; SQL 64 KiB; one parsed read query; explicit table
allowlist (including nested queries and CTEs); 1–1000 rows; approximately 512 KB row
payload; per-profile job concurrency; 30–600 second host timeout. The helper reserves
10 seconds for cleanup and attempts Athena cancellation on timeout/SIGTERM. Network
failure or forced host termination can prevent cancellation; configure workgroup
scan limits as a separate control. A row limit does not limit Athena bytes scanned.
HTTP connections have header/read/idle deadlines; async Athena tools avoid holding
an HTTP request open for query execution.

SQL validation uses sqlglot's Trino parser and rejects mutations, multiple statements,
unknown functions, and table functions. It deliberately supports a restricted subset
of Athena SQL; unsupported valid queries fail closed. Use a read-scoped AWS role with
only needed Glue/Lake Formation/S3 access, query-results writes, and workgroup access.
Do not grant external-function/federated Lambda invocation for this use case. Handler
validation complements AWS authorization rather than replacing it.

Security scope: one existing bearer token still authorizes all configured profiles
and job logs. This change does not isolate agents from existing build capabilities,
protect writable host scripts/configs, add log retention, or provision AWS IAM.

Tests (no AWS credentials required):

```bash
go test ./...
/absolute/host/path/athena-venv/bin/python -m unittest -v test_athena_runner
```
