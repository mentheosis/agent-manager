Implementation update: team startup wiring has been repaired in the working tree.
See [../README.md](../README.md) for the current runtime contract and manual checks.
The architecture findings below record the pre-repair baseline. Task queue
implementation has not started.

# Generic task-queue orchestration

Status: agreed direction; implementation plan, 2026-09-10.
This document authorizes no deployment or destructive database migration. Current
work is documentation. Implement in order: clarify/repair existing team orchestration,
then introduce task queues. Preserve unrelated working-tree changes.

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

## Existing architecture and cleanup findings

Inspection of this checkout and the installed executable found:

| Component | Intended responsibility | Observed wiring |
|---|---|---|
| `src/agent_manager/orchestrator.py` | Spawn/stop Go processes, track PID/port, capture output | Referenced by backend start/stop/restart routes |
| `orchestrator/loop.go` | Team discovery, leader prompting, status monitoring, idle nudges, pause/completion | Defines NewLoop, but no callers found |
| `orchestrator/main.go` | Executable entry point | Starts an MCP server only; does not construct/run Loop |
| `orchestrator/mcp.go` | LLM coordination tools and control callbacks | Present; loop callbacks need explicit wiring |
| `orchestrator/client.go` | Calls agent-manager conversation HTTP APIs | Reusable transport, subject to interface verification |
| `static/components/am-team-panel.js` | Parent/children UI and controller controls | Uses backend orchestration routes |

The Python launcher passes `--port`, `--task`, and `--agent`, whereas current main.go
and installed `am-orchestrator --help` expose `--base-url`, `--group`, `--mcp-port`.
The installed interface confirms this is not merely a source comment discrepancy.
Default API ports also differ between components. Explicit team MCP registration
was not found in the inspected Python provider path (Docker MCP registration exists).
Check all supported providers/configuration paths before classifying a component as
obsolete. Do not delete code based only on its name or old documentation.

First establish a working, tested team path with consistent CLI/API contracts. Treat
conversation status as liveness, not proof that a task has been completed.

## Reuse: share infrastructure, keep decision policies separate

There should be meaningful reuse, but no reason to force the two loops into one
large conditional state machine.

**Good candidates to share:**
- Conversation API client, session creation/send/read/interrupt operations.
- Stable controller/child identity and task-attempt metadata on conversations.
- Worker execution supervision, cancellation and normalized provider events.
- Structured result/progress submission transport and artifact references.
- Configuration validation, logging, metrics and process lifecycle.
- Parent/child navigation, conversation rendering, status/event UI primitives.

**Remain specific to teams:**
- Leader selection, team descriptions and inter-agent delegation/messaging policy.
- Feeding results to the leader and nudging an idle leader to make decisions.

**Remain specific to queues:**
- SQL claims, leases/fencing, dependencies, retries, reviews and durable attempts.
- Definition resolution, parameter validation and immutable input snapshots.
- Concurrency/resource budgets and recovery after scheduler restart.

The existing watcher can inform worker liveness, but ready/running status transitions
cannot replace durable queue outcomes. Extract shared packages only when both callers
need them; avoid a broad framework refactor before the vertical slice works.

## Proposed code organization

```text
orchestrator/
  main.go                         # explicit controller/MCP entry modes
  ...                             # existing team code, reorganized deliberately
  task_queues/
    PLAN.md
    protocol.md                   # authoritative SQL and worker protocol
    schema/                       # current generic MySQL DDL
    migrations/                   # ordered, reviewed schema migrations
    ... Go queue store, scheduler, definition resolver and tests
```

Shared Go packages can be extracted adjacent to this directory as needed. Keep one
Go module initially unless packaging constraints justify another. Prefer an explicit
`team` versus `task-queue` runtime mode of the supervised binary; preserve standalone
MCP mode if it has supported callers. Final CLI syntax is settled during cleanup,
with tests covering Python launcher compatibility.

## Canonical task definitions and parameters

Do not store copied instruction bodies in individual task records. Canonical task
definitions live in the consuming repository, not agent-manager. Each describes
objectives, required parameters, output format, permitted work and acceptance criteria.
Definitions can reference shared requirements maintained by that consumer.

A task stores a `definition_ref` resolving to repository identity, immutable commit
and relative path, plus optional integrity hash. Definition references may be inline
structured values or normalized generic registry rows; do not maintain two editable
sources of instructions. MVP uses repo files, without requiring a task-type table.

The generic resolver validates approved repositories/paths, pins the commit, and
loads the content as worker instructions. Pin referenced requirement files as well.
Reject traversal, unexpected symlinks and unapproved paths; do not execute setup code
merely because a task references it. Missing definitions block work rather than falling
back silently to the latest file. Local uncommitted definitions require an explicitly
captured immutable artifact, not a mutable path disguised as a version.

Parameters remain JSON, with validation against the definition's versioned parameter
schema. The scheduler understands structural validation only. Snapshot accepted
upstream artifact references/hashes into each attempt. Changing a definition or an
accepted upstream result requires explicit invalidation/revision of dependent work.

## SQL protocol

The generic DDL and migrations belong here. The initial target is MySQL 8.0; do not
claim database portability until adapters and concurrency semantics are tested.
Configure an approved table namespace and validate identifiers. Credentials are kept
in the controller environment/secret store, never tasks, prompts or logs.

Design these logical tables before implementing the loop (names finalized in DDL):

| Table | Purpose |
|---|---|
| tasks | Opaque type/workflow, pinned definition, parameters, dependency, order/priority, state, scheduling/retry settings, latest accepted result reference |
| task_attempts | Worker/conversation ID, controller identity, lease/fencing token, input snapshot, start/heartbeat/end, outcome/error and resource usage |
| work_log | Append-only meaningful events and concise reports, linked to task/attempt |
| schema_version | Detect incompatible controller/database versions |

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
   below attempt limit, required access available, capacity available.
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
execution, retry count, token/spend allowance and provider requests. Share limits across
controllers when they use the same provider/credential resource.

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
- No inline instruction copies in queue rows, mutable unversioned task definitions,
  or secrets passed to workers.
- No task completion inferred solely from process exit, UI ready state or LLM confidence.
- No broad host-command privilege added as a convenience for queue workers. The
  existing single-token command MCP is not an isolation boundary for a large pool.
- No automatic deployment/merge/publication without established authorization.
- No unrelated orchestrator/UI changes or speculative abstractions during cleanup.

## Delivery sequence

1. **Team architecture cleanup:** trace current callers/history, classify active versus
   abandoned paths, repair entry-point flags and loop wiring, verify MCP integration,
   start/stop/restart and visible output with a small team. Update architecture docs.
2. **SQL and worker protocol:** finalize generic DDL/migrations, pinned definitions,
   attempt/log schemas, claiming and submission semantics. Supply test fixtures with
   neutral example task types. Consumer-specific definitions remain outside this repo.
3. **Go vertical slice:** supervised task-queue mode claims one neutral task, starts
   a child session, records progress, receives a result and routes review/completion.
4. **UI integration:** queue creation/settings, task/activity views and nested attempts;
   pause/resume/cancel/review and reconnect behavior.
5. **Recovery and isolation:** multiple controllers, lease expiry, crash recovery,
   duplicate launches/submissions, stale workers, dependency invalidation and limits.
6. **Consumer migration/pilot:** migrate the existing small queue after canonical files
   are pinned; begin with low concurrency and human review. Increase capacity based
   on accepted work and operational reliability, not the number of spawned agents.

Acceptance tests include launcher/binary compatibility; team regressions; two
controllers claiming simultaneously; crash at each claim/spawn/submit boundary;
lease expiry and stale writes; provider failure; blocked dependencies; review rejection;
canonical definition changes; artifact immutability; SQL/credential redaction; UI
reconnect and accurate separation of generation/task/controller states.

Open details for implementation: final binary CLI modes, schema/table naming,
artifact storage backend, scoped worker submission transport, review representation
and exact generic definition manifest schema. These are interface decisions to settle
before dependent code, not reasons to add consumer-specific scheduler logic.
