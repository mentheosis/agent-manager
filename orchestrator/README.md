# Team orchestration

Python supervises one Go controller per team. The Go controller discovers the
leader, sends the saved task, polls child status, and nudges an idle leader to
review worker results. Each provider session remains a normal agent-manager
conversation, with its events available through the existing UI.

## Runtime modes

- `am-orchestrator --mode team --group TITLE --base-url URL --mcp-port PORT --task TEXT`
  runs the control loop and a localhost HTTP control endpoint. Python chooses the
  port and checks `/status` before reporting startup success.
- `am-orchestrator --mode mcp --group TITLE --base-url URL --managed`
  serves stdio MCP tools to the leader. Completion is forwarded through Python
  to the team's supervised controller, rather than stranded in a second process.
- Omitting `--mode` preserves standalone MCP mode. `--mcp-port` selects localhost
  HTTP instead of stdio. Standalone MCP without `--managed` has no team control
  loop; use team mode for autonomous orchestration.

The API URL defaults to localhost:8787. Python uses `AGENT_MANAGER_PORT`, or an
explicit `AGENT_MANAGER_BASE_URL` override. The old launcher flags `--port` and
`--agent` are replaced by `--mode team --mcp-port`; children are discovered from
agent-manager's registry.

Claude and Codex leaders receive the same stdio MCP server configuration. Claude
permits the five coordination tools explicitly. Tool requests targeting an agent
are restricted to current members of that team. The localhost HTTP endpoint and
existing agent-manager API remain trusted application interfaces, not isolation
boundaries for hostile code running in the container.

## Controls

Start requires a saved task, exactly one ready leader with the `orchestrator`
preset, and at least one worker agent. It reloads leader runtime options while
preserving the conversation so newly assigned team tools take effect. Repeated
Start calls are rejected while the controller is alive.

Pause suspends automatic leader updates. Resume continues the existing loop.
Completion sets the controller to `done`, leaving the process available for
inspection. Restart after completion sends the currently saved task again.
Stop terminates the controller. These controls do not abort agents already
working; use the individual conversation's abort control when needed.

The team's Conversation tab displays a dedicated activity feed: controller logs,
delegation messages, agent responses, status changes, and turn completion. It uses
the parent's persisted WebSocket event stream, with replay after refresh/reconnect.
Loop parents do not create LLM sessions. Existing teams without an activity history
are seeded once from retained child conversation events on application startup.
Individual agent tabs retain their normal conversations.

The team panel polls the controller's state separately from child conversation
states. Controller output is available at
`GET /api/instances/TITLE/orchestrator/output`. A completed controller can remain
alive while showing `done`; a stopped controller shows `stopped`.

Team membership is discovered at the start of each run. Stop the controller before
changing its leader or membership, then start it again. Automatic idle review uses
two consecutive 30-second heartbeat checks, so a worker finishing need not produce
an immediate leader update.

## Build and manual check

Rebuild and restart the agent-manager container through the usual deployment
command before testing: both the installed Go binary and Python application must
be updated together. Refresh the browser to load the updated team panel.
The Dockerfile already builds and installs `am-orchestrator`.

1. Create/open a team with one leader using the `orchestrator` preset and at least
   one worker. Wait until the leader is ready.
2. Save a small task such as: “Ask the worker to read this repository's README,
   report three facts, then review its response and mark the task done. Do not edit
   any files.” Click Start.
3. Confirm the leader receives the task and invokes `send_to_agent`; the worker's
   conversation should show the delegated prompt and its response.
4. Confirm the leader can read the worker's output and call `mark_task_done`, and
   the team panel eventually shows `done`. Allow roughly a minute for idle review.
5. On another run, test Pause/Resume. Pause should preserve the controller process;
   Resume should continue without resending the initial task.
6. Test Stop, including while a worker is active. The controller should stop promptly
   and the worker should remain independently visible. Save a new task and Start
   again after the leader becomes ready.

Automated checks (from the respective directories):

```sh
# orchestrator/
go test -race ./...
go build -o /tmp/am-orchestrator-teams ./cmd/am-orchestrator

# repository root, with Python development dependencies installed
AM_TEST_ORCHESTRATOR_BINARY=/tmp/am-orchestrator-teams pytest -q tests/test_orchestrator.py tests/test_team_api.py tests/test_claude_provider.py tests/test_codex_provider.py tests/test_instance_runtime.py tests/test_server.py
```

Tests cover controller startup, delegation, completion while paused, a subsequent
task, bind failure, completion forwarding, provider tool configuration, API controls,
invalid binaries, duplicate starts, and shutdown without waiting indefinitely on
the output reader. The binary integration test launches Go against a fake local API;
it makes no LLM calls. Actual provider authentication/tool execution is covered by
the manual check.

Generic SQL task queues remain future work, described in
[task_queues/PLAN.md](task_queues/PLAN.md).

## Task queues

Teams now live in `teams/`; shared transport and runtime live in this directory.
The executable is `cmd/am-orchestrator`. Generic SQL task queues live in
`task_queues/`; see the [protocol and setup guide](task_queues/protocol.md).
