# Generic task-queue orchestration

Status: v1 implemented and automated validation passed, 2026-09-11. The Go/team split,
generic SQL scheduler, provider adapters and task-queue UI are implemented. The
setup and current schema v1 contract are in [protocol.md](protocol.md). Existing consumer tables
have not been migrated, and no live provider pilot has been launched.

Implementation decisions:
- Tables are `am_tasks`, `am_task_attempts`, `am_work_log`, and
  `am_schema_version`, with a configurable prefix and explicit schema setup.
- Definitions are embedded in deployment profiles, keyed by queue ID and task type,
  with Markdown instruction paths and a parameter map. Workspaces are isolated Git archive exports; output artifacts are copied
  to a content-addressed local store.
- Worker reporting uses attempt-scoped MCP tools backed by authenticated HTTP.
- Review is human-gated in v1. Automatic review promotion remains a later extension.
- Concurrent controllers share one backend and durable state; distributed backends
  and cross-queue provider quota coordination are outside v1.
- The parent UI combines tasks/activity with status/settings in the right panel.
  Controller deletion preserves task/attempt history and worker conversations; direct
  attempt deletion remains disabled.
- Wall-time and reported token thresholds are implemented; provider account
  quotas remain necessary for hard spending limits.
- Python team compatibility routes remain in the shared API; leader MCP setup is
  in the team adapter. Further mechanical route extraction can happen independently.

The sections below preserve the design and release gates. Consumer migration,
manual team verification and a low-concurrency live provider pilot are deployment
steps, not actions performed automatically by this implementation.

## Intent

Agent-manager supports two forms of orchestration:

- **Team leader:** an LLM leader delegates and coordinates workers around a goal.
- **Task queue:** a deterministic Go controller claims durable SQL tasks and launches
  LLM workers using canonical, versioned task definitions.

Both appear as parent controllers with nested worker conversations. Operators can
observe intermediate assistant output, tool activity, concise progress reports and
results without making a parent LLM responsible for scheduling reliability.

Task queues live under `orchestrator/task_queues`. The control loop is Go. Python
continues to own the web/API layer, conversation/provider sessions, persistence of
instances, and supervision of controller processes. Do not implement a second queue
scheduler in Python or in an LLM prompt.

## Domain independence is mandatory

Agent-manager has no knowledge of any consuming project's chains, accounting,
ETLs, schemas, task types, business logic or acceptance rules. It must not contain
project-specific imports, SQL filters, prompts, status transitions or provider logic.

A consuming project supplies canonical definitions, task parameters, dependencies,
evidence and domain-specific review criteria. The agreed SQL protocol is the durable
integration boundary. Agent-manager owns that protocol and its migrations; the
consumer can host the tables in its own database.

Task type identifiers, workflow IDs and parameters are opaque to the scheduler.
For example, `parameters` may contain a symbol or a replay period, but these must
not become scheduler columns or special cases. Connection schema/table namespaces
are deployment settings; never hardcode a consumer database name into shipped DDL.
This is a versioned SQL contract, not support for arbitrary preexisting table layouts.

## Pre-implementation source audit

The following records the source audit that motivated this change, not which binary is
installed or deployed. The earlier startup failures are repaired: `main.go` now
runs the team loop, Python uses compatible flags and the configured API URL, leader
MCP tools are registered for Claude and Codex, and completion reaches the controller.
The UI has a replayable team activity feed, scoped filters and working sidebar collapse.

| Current location | Finding | Refactor decision |
|---|---|---|
| `main.go`, `loop.go`, `mcp.go`, `watcher.go` | All Go code is `package main`; `runTeam` combines process lifecycle, HTTP serving and team callback wiring | Make parent directory an importable library; move entry point into `cmd/am-orchestrator`; team wiring/decisions into `teams` |
| `client.go` | Useful list/get/send/history/children API transport; concrete HTTP client, no request contexts or create/recover attempt API | Share transport and wire types; inject a narrow client interface; add context cancellation before queue use |
| `protocol.go` | Mixes generic event/history shapes with team `StatusUpdate`, `AgentStatus` and leader prompt formatting | Split generic wire models from team prompts/status reports |
| `mcp.go` | Mixes JSON-RPC framing, HTTP control routes and five leader tools; managed stdio completion forwards through Python | Keep tool definitions, membership policy and completion handling in teams; share transport only when queue submission has a concrete consumer |
| `watcher.go` | Polling is tied to group titles and team error summaries; recent errors can be reported repeatedly | Keep in teams initially; queue health checks must not inherit leader notification policy |
| `config.go`, `presets.go`, `go.mod` | `BatchWindow`, `GroupTitle`, `MCPPort` have no consumers beyond declarations/defaults; preset helper methods have no callers found; websocket dependency has no imports found | Verify full call sites, remove unused config and dependency during cleanup; retain embedded leader instructions and move presets with teams |
| Python `orchestrator.py` | Working PID/port/readiness/shutdown supervision, but hardcodes team CLI and publishes `team_event` | Shared supervisor plus mode-specific launch specs and event adapters |
| Python `instance.py`, providers | Loop parents no longer run LLMs; leader-specific MCP construction accesses supervisor internals; provider config field is `team_mcp` | Move leader policy into team adapter; expose public launch/config interfaces and a provider-neutral MCP config map |
| Python `state.py`, `server.py`, persistence | `kind=loop` and legacy aliases imply team everywhere; team preflight, forwarding/backfill and routes are embedded in shared code | Add explicit controller mode; extract adapters while preserving old records/routes |
| UI `am-loop-pane.js`, `am-team-panel.*` | Activity renderer and controls work but both are team-specific despite the generic loop name | Move team views under `components/teams`; extract generic event presentation without inheriting team semantics |
| UI `am-new-dialog.js`, `am-toolbar.js`, `am-sidebar.js`, `am-app.js` | Team creation, filter labels, routing, drag/drop and expansion are spread across shared components | Keep generic shells in parent; move team definitions/forms/actions to teams, introduce queue equivalents |

### Issues identified during the audit

- **Command acknowledgements:** `/task` currently returns success even when its
  buffered channel is full, and the active run does not consume that channel.
  Team completion has a similar buffered signal. Document legal transitions and
  reject unsupported/busy commands explicitly; do not promote this into a shared
  durable command/submission protocol. Queue outcomes require attempt fencing.
- **Team lifecycle semantics:** membership is discovered at run start, idle checks
  use fixed heartbeat intervals, and pause suppresses automatic updates rather than
  stopping agents. Keep these semantics and controls explicit; queue pause means
  stop dispatching while attempts continue. Do not share a mode-specific state enum.
- **History fidelity:** `read_agent_output` no longer truncates message text, but the
  Go `Event` model omits tool arguments, error `message`, sequence and other provider
  fields. Shared history models must preserve needed fields (including unknown/raw
  payload where useful). Check the advertised event ordering against the backend.
  A formatted history string is not a task result API.
- **Team creation:** the browser performs multiple create/reparent/preset requests;
  some failures can leave a partial team. During team extraction add validation and
  explicit failure reporting; do not use this flow to create durable queue attempts.
  Queue launch must have an idempotent backend operation keyed by attempt ID.
- **Controller events:** stdout is wrapped as a generic team log message, while
  child activity has actor/subtype fields. Queue events need structured task/attempt
  IDs and SQL work-log IDs, not parsing English stdout. Diagnostic output remains
  useful but cannot be the queue's durable source of truth.
- **Polling UI:** team controls rerender on polling. Preserve unsaved edits,
  expand/collapse state and scroll position during extraction; avoid reproducing
  polling-driven full renders for task lists.

These findings do not justify rewriting the working team scheduler. Separate
mechanical moves from behavior fixes so existing team behavior remains reviewable.

## Reuse and dependency direction

Share conversation transport/models, cancellable HTTP requests, localhost controller
serving/readiness, process supervision, normalized event presentation, parent/child
navigation and provider execution. Keep leader selection, prompts, membership policy,
idle nudges and team MCP tools in teams. Keep SQL, claims, leases, attempts, definition
resolution, dependencies, review policy and recovery in task queues.

The existing watcher stays in teams until a concrete second use justifies extracting
its polling mechanism. Do not make queue task success depend on ready/running state.
Do not create a universal scheduler or plugin framework as a prerequisite.

Go dependency direction is `cmd -> teams/taskqueues -> parent shared library`.
The parent package never imports either mode; teams and taskqueues never import each
other. Shared interfaces describe capabilities (run, state, event sink, conversation
access), not team concepts such as leader, done channel or restart prompt.

## Target file organization

### Go: one module, shared library in the parent

```text
orchestrator/
  go.mod / go.sum
  README.md
  cmd/am-orchestrator/main.go       # flags, mode selection, signal handling
  client.go                        # package orchestration: conversation HTTP client
  models.go                        # conversation/history wire types, not team prompts
  runtime.go                       # localhost serving, readiness, shutdown support
  events.go                        # structured controller event envelope/sink
  *_test.go                        # shared infrastructure tests
  teams/                           # package teams
    run.go                         # constructs loop and wires team controls/MCP
    loop.go
    watcher.go
    config.go
    protocol.go                    # leader updates and prompt formatting
    mcp.go                         # team tools and managed completion adapter
    presets.go
    presets/*.md
    *_test.go
  task_queues/                     # package taskqueues
    PLAN.md
    protocol.md
    schema/
    migrations/
    run.go
    config.go
    scheduler.go
    store.go                       # MySQL claims, transitions and ownership checks
    definitions.go                 # profile task resolution, params and input snapshots
    submissions.go                 # authenticated attempt progress/results
    recovery.go
    *_test.go
```

Names for queue implementation files describe responsibility; create them with their
implementation, not as empty scaffolding. A shared `mcp_transport.go` can join the
parent only if queue submissions actually use MCP. Move team MCP intact first;
its tool registry and authentication/authorization decisions are never shared policy.

The executable must move because Go cannot import `package main`, and two packages
cannot occupy the same directory. Preserve the installed name `am-orchestrator`.
Update Docker builds from `go build .` to `go build ./cmd/am-orchestrator`; retain
`go test ./...`, update local build instructions and integration fixtures. Keep the
repo binary fallback path explicit; moving source must not break Python discovery.
Move embedded preset files with their embed declarations and verify build context.

Keep existing `--mode team`, `--mode mcp`, `--managed`, `--group`, `--base-url`,
`--mcp-port` and `--task` compatibility during refactor. Add `--mode task-queue` only
with queue implementation and its validated config. The generic supervisor passes
mode-specific arguments supplied by an adapter. Credentials must not be CLI arguments.

### Python: shared supervision with small mode adapters

```text
src/agent_manager/
  orchestrator.py                   # retain public supervisor compatibility
  orchestration/
    controllers.py                 # launch specs, mode dispatch, public interfaces
    events.py                      # common event envelope/dispatch
    teams.py                       # preflight, MCP config, controls, child forwarding
    task_queues.py                 # queue config/control/submission HTTP adapter
  server.py                        # mounts routes, remains application entry layer
  instance.py / state.py            # generic session/controller identity and lifecycle
  providers/                       # generic MCP configs + existing provider execution
```

Python task-queue code must not implement SQL eligibility, claim/retry or lease renewal.
It authenticates/validates HTTP input and delegates to Go. Avoid importing private
supervisor members from provider/instance code. Keep team routes as compatibility
adapters while generic controller routes are introduced for the new mode.

### UI: specific views in subfolders, shared components in the parent

```text
static/components/
  am-app.js                        # routes based on controller mode
  am-new-dialog.js                 # creation dialog shell
  am-toolbar.js                    # shared filter/action menu rendering
  am-sidebar.js                    # generic parent/child tree
  am-terminal-pane.js / .css        # normal worker conversations
  am-controller-activity.js / .css  # shared colored/collapsible event presentation
  teams/
    am-team-panel.js / .css         # existing task/member/controller sidebar
    am-team-activity.js             # replaces team-specific am-loop-pane implementation
    am-team-create.js               # YAML defaults, validation, creation flow
    view-config.js                 # team filters, labels, actions
  task_queues/
    am-task-queue-panel.js / .css
    am-task-queue-create.js
    am-task-queue-tasks.js
    am-task-queue-activity.js
    view-config.js
```

Use explicit imports and small configuration objects, not a new UI plugin system.
Common activity rendering accepts normalized events and filter definitions; each
mode controls its own source, labels, retention and replay cursor. Team transcripts
remain file-backed; queue lifecycle/work logs remain SQL-backed. Do not duplicate
SQL lifecycle events through both stdout forwarding and work-log streaming.

Preserve current typography, inline timestamps, actor/target labels, colors,
keyboard-accessible expand/collapse, reconnect deduplication and independent filters.
The sidebar groups any controller's children; only the teams adapter decides team
membership actions. Queue attempt ownership must not be editable through generic
team drag/drop or Add Agent controls.

Move `am-team-panel.css` and update `static/style.css`. Move inline activity CSS into
the shared stylesheet. Update `am-app.js` imports, nested relative `../../lib/` paths,
custom-element registrations, test fixture paths and any selector coupling. Preserve
existing URLs (`conversation` tab) and use temporary re-exports/element aliases only
where needed; do not register a custom element twice. Generic new-dialog keeps agent
and batch modes; team YAML and queue connection forms belong to their own modules.

## Controller identity and compatibility

Retain `kind=agent|loop` initially and add `controller_mode=team|task_queue` for loops.
This minimizes changes to existing parent/child behavior. Normalize old loop records
without a mode to `team` at the persistence/API boundary, not in scattered components.
Provider identifies an actual LLM session; it must not select controller behavior.
Queue parents require no leader, LLM model, team task text or team preset.

Expose mode in API summaries and use it for controls, filtering, MCP injection,
reparent/delete policy and dispatch. Guard existing team-specific routes against
queue parents. Legacy team aliases remain readable during migration. Do not send
queue children through team history backfill or infer queue outcomes from their chats.

Add durable controller/conversation IDs and immutable task/attempt correlation before
queue launch. Keep title-addressed team APIs working; display labels and old titles
must not become SQL ownership/fencing tokens. Persist correlation before starting a
worker so crash recovery can locate the conversation without duplicate creation.

Use a versioned controller event envelope with mode, controller ID, actor, target,
subtype, timestamp and optional task/attempt/work-log references. Adapt existing
`team_event` records on read; avoid destructive transcript rewrites. Separate the
browser stream cursor from the SQL work-log cursor and attempt fencing token.

## Canonical task definitions and parameters

Do not store copied instruction bodies in individual task records. Canonical task
definitions live in the consuming repository, not agent-manager. Each describes
objectives, required parameters, output format, permitted work and acceptance criteria.
Definitions can reference shared requirements maintained by that consumer.

Profiles support use_isolated_workspace (default true). False launches in the
existing approved repository, with live files and edits. Inputs remain in a separate
attempt-specific state directory. Retries preserve instructions/configuration but
cannot reproduce shared filesystem state. Serialize shared writers across controllers.

The deployment profile name is the queue ID; its embedded tasks map defines canonical
task types. Each entry names an absolute Markdown instruction path, provider, model,
permission and parameter map. There is no per-task definition_ref or manifest file.
The resolver captures the exact instructions, task configuration and Git HEAD source
archive before execution. Full inputs are stored in am_task_attempts.input_snapshot.
Retries retain the preceding snapshot; new task rows resolve current definitions.
Only the repository containing the instructions is exported into a worker workspace.

Table names are static. Storage is derived from AGENT_MANAGER_STATE_DIR/queue-work/
<queue_id>. Profiles specify database_env and default_max_workers/default_lease_secs/
default_task_limit_secs/default_task_limit_tokens. Creation and the right panel allow
controller-specific overrides. Running attempts and retries retain original limits.
See protocol.md for the authoritative implementation contract and initial_manual_test_plan.md
for the first deployment procedure.

## Rendering and file contracts

Stage Markdown and inputs/outputs support literal {{parameter}} substitution using
validated parameters. Inputs may use upstream:<path> for accepted predecessor files.
The Go controller validates inputs before launch, records signatures, rejects missing
or stale required outputs on completion, and archives declared outputs automatically.
File validation does not judge semantic correctness. Blocked/failed results can carry
partial evidence.

Operator CLI render/enqueue and a UI Load tasks dialog provide preview and batch
insertion without starting a controller. Batches use stable producer keys/request
hashes, atomic insertion and explicit dependencies. The initial am_tasks DDL includes
producer_key/request_hash and a unique producer-task key for repeat-safe loading.

## SQL protocol

The generic DDL and migrations belong here. The initial target is MySQL 8.0; do not
claim database portability until adapters and concurrency semantics are tested.
Configure an approved table namespace and validate identifiers. Credentials are kept
in the controller environment/secret store, never tasks, prompts or logs.

Design these logical tables before implementing the loop (names finalized in DDL):

| Table | Purpose |
|---|---|
| am_tasks | Canonical type/workflow, parameters, dependency, order/priority, state, scheduling/retry settings, latest accepted result reference |
| am_task_attempts | Worker/conversation ID, controller identity, lease/fencing token, input snapshot, start/heartbeat/end, outcome/error and resource usage |
| am_work_log | Append-only meaningful events and concise reports, linked to task/attempt |
| am_schema_version | Detect incompatible controller/database versions |

A single predecessor is sufficient for the initial linear workflows. If multi-parent
tasks are supported, use a dependency table; do not serialize executable dependency
logic in prompts. Order is display/scheduling preference, not dependency enforcement.
Validate cycles and cross-workflow dependencies according to the documented policy.

Work-log fields: ID, task ID, optional attempt ID, actor/worker, event type, timestamp,
concise summary and artifact references. Include started, progress, finding, blocked,
submitted, review and retry/cancel events. Heartbeats update attempt liveness without
flooding the log. Store review actor, decision and exact reviewed output version.
Do not store private chain-of-thought; logs describe actions, evidence and outcomes.

Keep complete outputs as immutable artifacts; task rows can retain a small structured
summary or accepted result pointer. Attempts preserve prior outcomes rather than
silently overwriting failed worker reports. Define retention, sensitivity, access and
size limits separately from conversational transcript retention.

Migrate existing consumer task tables explicitly: move domain fields into parameters,
replace copied instructions with definition references, preserve IDs/dependencies and
existing outputs. Back up and validate before removing old columns or constraints.
No automatic destructive migration on controller startup. Do not pin a nonexistent
commit for definitions that have not yet been committed.

## Deterministic control loop

1. Confirm protocol version, configured queue scope and resource limits.
2. Find eligible tasks: queued/retry-ready, available now, dependencies accepted,
   below attempt limit and required access available; separately enforce the persisted
   `max_workers` capacity limit when reserving a claim.
3. Atomically claim under a short SQL transaction (row locking / SKIP LOCKED), create
   an attempt and issue a fresh fencing token. Multiple controllers must not double-claim.
4. Resolve/snapshot definition, parameters and upstream artifacts. Create a fresh child
   conversation/workspace for the attempt, linked by immutable IDs, then launch.
5. Monitor real worker/session health and budgets. Renew the lease only while the
   controller maintains valid ownership and execution is within policy.
6. Accept structured progress/result submissions through an authenticated,
   attempt-scoped interface. Validate token, lease, payload and artifact references.
7. Record outcome and transition to awaiting_review, completed, blocked, retry_wait
   or failed according to generic policy. A normal final chat message is not sufficient.
8. Continue dispatching until paused, stopped, out of capacity or no eligible work.

Create-session/start operations need an idempotent attempt correlation key. If the
controller crashes after spawning but before recording a session ID, recovery must
find that session rather than blindly spawn a duplicate. Use stable IDs, not titles,
for durable relationships even if legacy APIs currently address instances by title.

Queue dispatch is at-least-once. Enforce token/revision checks on heartbeat, progress,
completion and result promotion, rejecting expired/stale workers. SQL fencing does
not by itself fence external writes: isolate artifacts/worktrees by attempt and
require idempotency or separate authorization for external side effects.

On restart reconcile durable attempts with actual session/process existence: attach
to surviving valid workers, classify ended workers, and recover expired leases.
A live controller must not extend a hung worker indefinitely. Bound wall time, idle
execution, retry count, token allowance and provider requests. Limits are per
controller; provider-account quotas are a deployment concern, not a shared scheduler cap.

## Parallel task limit and right-side queue controls

Each UI controller has a locally persisted `max_workers` setting: the maximum number
of concurrent worker attempts it owns. Tasks use the profile name as `queue_id`, with no queue
registration table or foreign key. Producers can insert tasks before any controller exists. Default to **1** for initial
rollout; operators can set **2** or increase it later. Validate positive integers. Use Pause to suspend dispatch rather
than giving zero an ambiguous meaning. Include the setting in queue creation and
configuration as well as the running controller's right-side panel.

The right-side panel must show:

- **Available tasks:** tasks eligible to claim now, before applying the worker limit
  or pause state. Exclude unmet dependencies, pending review, blocked tasks, future
  availability/retry times and exhausted attempts. This shows the ready backlog even
  when all worker slots are occupied.
- **Active tasks:** tasks owned by this controller with an in-flight worker attempt, including claimed/starting
  attempts and workers awaiting a validated outcome. An idle/ready conversation alone
  does not release its slot. Display active worker usage alongside the limit, such as
  `Active workers: 2 / 2`.
- **Max workers:** editable numeric control with an explicit Apply action, validation,
  and saving/error feedback. Show the persisted effective value after acknowledgement;
  a browser edit alone must not change the displayed effective scheduler limit.

Count every launched LLM worker, including automated review workers, against the
same worker cap. Human review waits use no worker slot. If worker usage differs from
active task count, display both rather than presenting them as interchangeable.
Counts and effective configuration come from the backend for the same queue scope,
refresh while the panel is open, and recover after reconnect. Label stale/unavailable
counts explicitly instead of displaying zero.

The Go scheduler enforces the cap **before claiming/launching**, reserving a slot
atomically with the attempt claim. Eligible backlog must never cause extra workers
to launch. Limits and pause state are per controller, saved under its workspace
root with its stable controller ID. Two controllers at limit 2 can run 4 total.
SQL row locks prevent duplicate task claims. Restarting the same controller retains
its settings and counts its existing attempts; a newly created controller starts
with its own defaults. Available backlog is pool-wide; active counts are local.
Deleting a controller stops only its own workers and preserves SQL history,
artifacts and worker conversations. No pool-wide capacity coordinator is required.

Changing the limit does not restart the controller. Raising it allows more work on
the next scheduling cycle. Lowering it lets existing workers finish and prevents new
claims until usage falls below the new limit; never cancel active work implicitly.
Record the actor, old/new limits and timestamp in the queue work log. Pause/resume
preserves the limit, and controller restarts restore it. Deployment/provider resource
limits can further restrict dispatch but cannot increase this queue-level cap.

## State and review semantics

Task states: queued, running, awaiting_review, completed, blocked, retry_wait,
failed, cancelled. Optional superseded state/revision is resolved in protocol design.

- Worker "ready" means it is not generating; it does not mean task success.
- Required review gates dependencies: awaiting_review is not completed.
- Retry transient service failures with bounded backoff; missing credentials, unknown
  requirements and unavailable data are structured blockers with unblock conditions.
- A worker may propose follow-up tasks but cannot grant itself tools, spending or
  acceptance. Policy/reviewer decides whether to enqueue proposals.
- Reviewers can be human or separate LLM sessions. Record provenance and evidence;
  structural output validation cannot establish domain correctness.

A result references outputs, a concise summary, outcome and blockers. Domain checks
are supplied by definitions and executed only through approved capabilities; do not
turn arbitrary definition text into privileged host commands.

## UI and conversation lifecycle

Add a Task Queue controller type alongside Team. Queue parent is a control/status
view, not a mandatory LLM conversation. Optional conversational triage can be added
later without making it the scheduler.

Parent view: Overview, Tasks, Activity, Settings. Show counts by state, blocked reasons,
review requests, active attempts, capacity and controller health. Work-log summaries
form the concise activity feed. Existing conversation UI shows intermediate assistant
messages, tool calls/results and final submissions under the parent.

Each attempt gets its own child conversation, e.g. Task 42 / Attempt 2. Reviews can
have their own child conversations. Keep previous attempts inspectable; don't reuse
one long-lived worker conversation across unrelated tasks. Support filters/collapsed
history so a large completed queue does not overwhelm navigation.

Controls:
- Start/resume: dispatch eligible work.
- Pause dispatch: let active workers finish; stop assigning new tasks.
- Cancel attempt/task: explicit, separate action with recorded outcome.
- Stop controller: define graceful drain versus forced shutdown; preserve DB state.
- Retry/unblock/review: explicit audited transitions, not manual hidden row edits.

Stream controller/work-log events through the backend with durable sequence cursors
and reconnect catch-up. Browser tabs do not own leases. Distinguish controller state,
task state and LLM generation state in API/UI. Sensitive connection configuration
must never appear in browser payloads.

## Boundaries

- No consuming-project details in agent-manager implementation, built-in prompts or DDL.
- No inline instruction copies in task rows or secrets passed to workers. Definitions
  may change between tasks, but execution uses immutable per-attempt snapshots.
- No task completion inferred solely from process exit, UI ready state or LLM confidence.
- No broad host-command privilege added as a convenience for queue workers. The
  existing single-token command MCP is not an isolation boundary for a large pool.
- No automatic deployment/merge/publication without established authorization.
- No unrelated orchestrator/UI changes or speculative abstractions during cleanup.

## Delivery sequence and gates

1. **Baseline and mechanical Go split.** Preserve the repaired behavior and regression
   suite. Move team files/presets/tests into `teams`, generic client/models into the
   parent library, and the executable into `cmd/am-orchestrator`. Update Docker,
   launcher fixtures and README in the same change. No queue behavior or SQL yet.
2. **Small infrastructure refactor.** Extract shared serving/supervision and cancellable
   client calls, separate team adapters, remove verified dead config/dependencies.
   Handle team command acknowledgement/history fidelity as explicit fixes, with tests.
   Add normalized controller mode and compatibility reads; preserve existing teams.
3. **UI organization and adapter extraction.** Move team components/CSS, extract event
   presentation and menu configuration, separate team creation from the dialog shell.
   Verify no visual/behavior regression. This completes the reusable foundation.
4. **SQL and worker protocol.** Finalize DDL, migrations, profile task definitions,
   attempt identities, scoped submissions, review transitions and recovery semantics.
   Define idempotent backend create/find/start operations before launching workers.
5. **Go queue vertical slice.** Add task-queue mode that claims one neutral task, creates
   a fresh child, records progress and accepts a fenced result into review/completion.
   Include lease expiry and duplicate-submission rejection in the first slice; they
   are correctness requirements, not later optimizations.
6. **Queue UI.** Add creation/settings, tasks/activity and nested attempts; audited
   pause/resume/cancel/retry/review, structured events and reconnect catch-up. Include
   the right-side available/active counts and live `max_workers` control in this slice.
7. **Recovery and concurrency proof.** Exercise multiple controllers, crashes at every
   claim/spawn/record/submit boundary, stale workers, hung sessions, dependencies,
   revision invalidation and resource limits before increasing concurrency.
8. **Consumer migration/pilot.** Pin canonical files, explicitly migrate the existing
   small queue, begin with low concurrency and human review. Domain definitions and
   data remain in the consumer repository.

Refactor acceptance: Go tests/race checks and real launcher/binary integration; team
MCP initialization/delegation/completion with both providers; complete long history
messages; pause/resume/stop; old persistence records; no parent provider sessions;
activity history/backfill/reconnect; scoped filters; team/folder collapse; preserved
YAML/model/permission defaults; browser routing and stylesheet/module loading after
file moves. Repeat the small manual team check before starting queue dispatch work.

Queue acceptance additionally covers schema compatibility, simultaneous claims,
lease expiry, stale writes, duplicate launches/results, review rejection, blocked
and cyclic dependencies, immutable artifacts/definitions, provider failures, secrets
redaction and separation of controller/task/conversation states.

Concurrency acceptance: with many eligible tasks, limits of 1 and 2 permit at most
1 and 2 active workers respectively, including concurrent claims by two controllers.
Test available counts while saturated/paused; claimed-but-not-started slots; automated
review slots; ready conversations without submitted outcomes; raising/lowering limits
mid-run; invalid edits and failed saves; persisted limits across restart; and no
implicit cancellation when reducing the cap below current usage.

The protocol choices above are resolved in [protocol.md](protocol.md). Automated
validation covers Go race checks, real MySQL concurrency/recovery and a neutral HTTP
controller replay, Python provider/API/persistence tests, and Chromium UI controls.
The neutral replay submits progress and a hashed report artifact through the actual
Go HTTP server and gates acceptance on review. Successor workspaces receive verified
copies of accepted evidence.

Remaining release work is deployment configuration, explicit consumer migration,
manual team verification and a live provider pilot at max_workers=1. Broader rollout
requires the separate isolation and provider-account quota controls described in the
protocol; it must not assume that attempt fencing is a hostile-worker sandbox.

## Worker/reviewer rounds

Implemented optional per-task iterative review; see [REVIEW_LOOP.md](REVIEW_LOOP.md)
for the configuration, durable state machine, budget behavior, initial schema setup,
and verification plan. The existing scheduler remains the sole execution authority.
