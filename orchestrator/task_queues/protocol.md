# Task queue protocol v2

The scheduler is generic Go code. The consumer owns task types, repository content,
parameters, domain review criteria, and enqueueing. Python supplies conversations
and authenticated controller-to-worker operations. The browser is an operator UI.

## Deployment and setup

Requires MySQL 8.0 (InnoDB), Git, the built `am-orchestrator` binary and an installed,
authenticated Codex or Claude provider. Run controllers for a logical queue inside
**one agent-manager backend**, sharing its durable state directory and workspace
root. Controllers independently limit and pause their own workers. Atomic task-row
claims prevent duplicate assignment across controllers. Attempts record a backend
secret fingerprint so recovery only cancels workers through their original backend.
Keep that secret durable. Multi-backend recovery is outside the supported deployment.

1. Build the new binary: from `orchestrator`, run
   `go build -o am-orchestrator ./cmd/am-orchestrator`, or rebuild the container.
2. Copy [examples/profiles.json](examples/profiles.json) to a protected deployment
   file and configure the repository/workspace paths. Set `AM_TASK_QUEUE_PROFILES`
   to that absolute filename in the agent-manager environment. Profiles select one
   provider/model/permission mode; task data cannot override them.
3. Set the profile's named DSN environment variable through your existing secret
   configuration. It uses the Go MySQL driver format
   `user:password@tcp(host:3306)/database?tls=true` (TLS settings must match your server).
   Never put the connection string in task data or browser configuration.
4. Explicitly initialize the new protocol tables:
   `python -m agent_manager.orchestration init-schema --profile sample`.
   `--binary /absolute/path/am-orchestrator` selects a local build if needed.
   This runs [schema/002.sql](schema/002.sql), replacing the validated table prefix.
   Startup checks the version; it does not migrate existing consumer tables.
5. Commit canonical consumer task files and insert tasks with a chosen `queue_id`.
   No controller or registration row is required before inserting tasks.
6. In **Create New → Task Queue**, choose the profile, enter the matching queue
   identifier and initial worker limit (default 1), then start. Begin with review on.

Schema version 2 removes queue registration. Existing version 1 installations require
[explicit migration](schema/migrate_v1_to_v2.md); startup does not migrate tables.
Consumer-specific legacy tables are not automatically converted.

## Tables and producer contract

| Table | Owner and purpose |
| --- | --- |
| `am_schema_version` | Explicit schema installer; version compatibility |
| `am_tasks` | Producer inserts tasks; scheduler controls lifecycle and accepted result |
| `am_task_attempts` | Scheduler; unique attempt, owner, lease, conversation, immutable inputs and submitted result |
| `am_work_log` | Append-only scheduler/worker/operator reports and transitions |

All table names use the deployment prefix (`am_` by default). The prefix accepts
only letters, digits and underscores, starts with a letter, and is at most 41
characters. All timestamps use UTC with microsecond precision.

A producer inserts only its queue's task inputs and may revise them **before the
first claim**. After `attempt_count > 0`, inputs and dependency relationships are
immutable by protocol. A changed definition, parameter or upstream result requires
a **new workflow revision and new task rows**, never editing completed rows or
repointing already-running dependents. Application/database access controls should
restrict producer lifecycle writes; direct SQL modification can violate the protocol.

Example, before or after creating a controller (replace the commit and path with real
committed files in the approved repository):

```sql
INSERT INTO am_tasks
  (queue_id, workflow_id, task_type, task_order, definition_ref, parameters,
   review_required, max_attempts)
VALUES
  ('sample', 'pilot-001', 'report', 10,
   JSON_OBJECT('repository', 'sample', 'commit', '<full-40-character-commit-sha>',
               'path', 'tasks/report/task.json'),
   JSON_OBJECT('topic', 'the project README'), TRUE, 3);
```

`task_order` and `priority` affect scheduling preference, not dependencies.
`depends_on` is a single predecessor in the same queue and workflow. It must be
completed, including required review. Cycles and cross-workflow references block
queued tasks with an explanatory work-log event. Producer inserts must be idempotent
using its own stable IDs or insertion transaction; `workflow_id` itself is not a
unique task key. This protocol does not infer domain task uniqueness.

## Definitions and workspaces

`definition_ref` is `{repository, commit, path}`. Repository is an approved profile
key, commit is a full immutable 40-character Git SHA, and path names a JSON manifest.
The manifest contains `instructions` (a relative Markdown path) and inline
`parameters_schema` (JSON Schema); see [examples/task.json](examples/task.json).
External JSON Schema URL references are disabled. Referenced repository requirements
are available from the same commit in the workspace.

Before execution, the scheduler validates parameters and creates a private
`workspace_root/attempts/<attempt-id>` export of the pinned repository. This is a
Git archive export, **not a checkout with a writable shared Git directory**. It runs
no checkout hooks, filters or setup commands. Archives with links, special files,
path traversal, more than 100,000 entries or more than 1 GiB are rejected. Definition
files are limited to 2 MiB. An interrupted export is rebuilt before any worker launch.
Missing/invalid definitions block the task.

Accepted predecessor artifacts are hash-checked and copied read-only into the new
workspace under `.queue-inputs/<original-relative-path>`. Missing or mismatched bytes
block launch. The consumer repository must not occupy the reserved `.queue-inputs`
directory. Workers can inspect inputs without reading another attempt's workspace.

Attempts retain the definition reference and SHA-256, parameters, accepted predecessor
result, rendered prompt, workspace and conversation UUID. Retries get new attempts,
conversations and directories. Workers should include changed source files or a patch
as submitted artifacts; unsubmitted workspace changes are not accepted deliverables.

## Dispatch, recovery and capacity

A transaction claims one eligible task with `FOR UPDATE SKIP LOCKED`. Claim and
attempt insert commit together. A controller serializes its own claims and counts
its live attempts before claiming. Claimed, running and submitted attempts retain
slots until execution ends or cancellation is confirmed. One process lock prevents
two loops running the same controller identity from the same workspace root.

`max_workers` defaults to 1, is positive and bounded by the deployment ceiling.
The limit and pause state belong to the ephemeral UI controller and persist in
`workspace_root/controllers/<controller-id>.json` across its restarts. A new
controller gets new settings. Two controllers with limit 2 may run 4 workers total.
Raising a limit permits more claims; lowering it lets current workers finish.
The right panel shows eligible backlog across the pool, active tasks/workers owned
by this controller, and its effective limit. Unavailable counts never display as zero.

`queue_id` is an opaque pool identifier, not a registered object or controller ID.
Profiles configure execution and database access; the UI supplies the identifier.
An optional profile `queue_id` remains a default for older profiles.

Launch uses an attempt ID as its idempotency key. The backend persists the conversation
reservation and prompt hash before starting the provider. Repeated launch requests
return that conversation; changed prompts are rejected. A crash in the ambiguous
reservation/start window fails that attempt rather than resending potentially executed
work. Cancel persists a tombstone before acknowledgement, including when launch has
not yet arrived. Other controllers in the same backend can find the original worker.

After an unexpected controller exit, use Start/resume to restart it; automatic
process restart is left to deployment supervision in v1. Scheduling and budget
checks require a live controller. A restarted controller reconciles persisted attempts. Expired attempts retain capacity
until the backend confirms cancellation. An unreachable backend therefore stalls
capacity safely. No terminal outcome is inferred from `ready`: an ended worker without
a structured result fails, with bounded retry/backoff. Transport failures retry in the
next scheduling cycle. Retry delay is `min(300, 5 * attempt_count)` seconds; attempt
budget defaults to 3. A valid completion releases the slot only after execution ends.

The wall-time deadline defaults to 3,600 seconds; lease defaults to 60 seconds.
Reported token/cost thresholds default to 200,000 tokens / USD 20 per attempt and
block the task on breach. Usage is recorded on attempts. **Usage checks depend on
provider reporting and can overshoot; they are not hard billing limits.** Use provider
account quotas for hard spending limits. There is no cross-queue provider-account
quota coordinator in v1. Keep aggregate concurrency within deployment capacity.

## Worker submissions, artifacts and review

Workers receive only the attempt-scoped `queue_progress` and `queue_submit_result`
MCP tools from this scheduler. The provider may retain its own configured tools.
No automatic Docker host-command MCP is added for queue workers. Queue DSN variables
and controller configuration are removed/blanked from provider subprocess environments.

The attempt capability is HMAC-derived from a persistent supervisor secret and the
random attempt ID. It never appears in prompts or task rows. The server checks the
capability, active state and unexpired lease transactionally. Workers cannot approve
results, change limits or submit for another attempt through these tools.

- `queue_progress({summary})`: concise action/finding/evidence, maximum 4,000 bytes.
- `queue_submit_result({outcome, summary, artifact_paths?, blockers?})`:
  outcome `completed`, `blocked` or `failed`; summary maximum 16,000 bytes.
- HTTP/MCP submissions are bounded to 1 MiB. Exact repeated final payloads are
  acknowledged idempotently; different subsequent results conflict. No writes occur
  on an acknowledged duplicate, even if its original lease has since expired.
- Artifact paths must be relative, regular files contained in the attempt workspace.
  Maximum 50 files, 16 MiB each, 64 MiB total. The server copies bytes into a SHA-256
  addressed store and records verified hashes/sizes. A later workspace edit cannot
  change accepted bytes. Artifacts download as attachments from the task view.

Completed worker results either complete the task or enter `awaiting_review`.
Human review is the v1 built-in gate; approval includes the exact latest attempt ID,
records the actor/decision and promotes that submitted result. Rejection marks the
task failed; an explicit retry is allowed while budget remains. Automatic reviewers
may be ordinary queued tasks, but automatically promoting another task's result is
not a worker capability in v1. A review wait consumes no worker slot.

Logs contain actions and evidence, not private reasoning. SQL log IDs are replay
cursors, distinct from transcript sequence numbers. Task/activity polling catches up
from the last log ID; nested conversations retain normal assistant/tool output.

## Controls and trust boundary

Start/resume dispatches. Pause allows current work to finish. Drain and stop pauses
then stops the controller once its own workers finish. Resume cancels an outstanding
drain. Cancelling a running task first stops its worker; lowering capacity never does.
Retry/unblock and approve/reject are explicit logged operator actions. Generic send,
abort, reparent, permission changes and delete are disabled for managed attempts so
those actions cannot bypass attempt accounting. Deleting a controller stops its own workers and retains SQL history, artifacts and
worker conversations. If already stopped, outstanding leases are recovered by a
later controller after expiry. Deletion never removes the task pool or pauses peers.

The existing application is a trusted operator interface. Its browser/control APIs,
state directory, deployment profiles and artifact store must be protected by the
existing deployment boundary. Attempt tokens are **not a hostile-worker sandbox**:
agents with unrestricted access to the same user/filesystem/network could reach the
operator API, secrets or other artifacts. Isolated containers/users and authenticated
operator routes are required before running untrusted workers. This change neither
grants broad host access nor claims to close that pre-existing isolation gap.

SQL fencing cannot undo external side effects. Consumers must require idempotency
and appropriate authorization for writes outside the attempt workspace. Retain SQL
history, state/capability secret, provider transcripts and artifacts together; v1 does
not automatically garbage-collect them. Backups and artifact access policy belong to
the deployment.

## Validation

`go test -race ./...` tests teams, definitions, archive containment and artifacts.
The optional `python tests/browser_task_queue_smoke.py` uses Playwright Chromium
with a mock API to check browser rendering and controls without launching agents.
Set `AM_TEST_MYSQL_DSN` to a disposable MySQL database to also run claim/capacity,
lease, submission, review, retry and neutral replay tests. Tests create random prefixed
tables and remove them afterward; they do not use the configured production table prefix.

Build the binary and run Python tests with `AM_TEST_ORCHESTRATOR_BINARY` pointing to
it to exercise real Go process readiness/shutdown. `pytest` covers the Python API,
provider adapters and persistence. `node --test tests/*.cjs` covers controller routing,
filters, navigation and conversation behavior. A live provider pilot remains a manual
release gate; unit/integration tests do not launch paid LLM sessions.
