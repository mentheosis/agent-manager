# Task queue protocol v1

Agent-manager owns generic scheduling, attempts, reporting and operator controls.
Consumers own task instructions, parameters, domain review criteria and enqueueing.
This is the initial schema; no earlier deployment migration is needed.

## Profiles and setup

Requires MySQL 8.0/InnoDB, Git, the built `am-orchestrator` binary and authenticated
worker providers. Run controllers through one agent-manager backend with durable
state. Backend fingerprints fence recovery so another backend cannot pretend it
has stopped the original worker.

1. Build `go build -o am-orchestrator ./cmd/am-orchestrator` from `orchestrator`, or
   rebuild the container.
2. Put a deployment JSON file on the host, for example
   `~/.config/agent-manager/task-queue-profiles.json`. Mount it read-only into the
   container at `/etc/agent-manager/task-queue-profiles.json`, and set
   `AM_TASK_QUEUE_PROFILES` to that container path. See
   [examples/profiles.json](examples/profiles.json).
3. Each profile's `database_env` names an environment variable supplied to the
   agent-manager service. Its value uses Go MySQL DSN syntax, for example
   `user:password@tcp(host:3306)/database`. Configure TLS to match the deployment.
   Keep actual credentials out of profiles, task rows, prompts and the browser.
4. Initialize tables explicitly:
   `python -m agent_manager.orchestration init-schema --profile sample`.
   Optional `--binary /absolute/path/am-orchestrator` selects a local build.
   This applies [schema/001.sql](schema/001.sql); startup only checks compatibility.
5. Add task rows, then create a controller in **Create New → Task Queue**, selecting
   that profile. Inspect the populated defaults before starting.

A profile name is the `queue_id`. Its `tasks` map keys are canonical `task_type`
names. Together they resolve the task definition; they are not unique task-row keys.
No queue registration table or separate task manifest is needed. Multiple ephemeral
controllers can select the same profile/pool and independently limit their workers.

Each task definition supplies:

- `instructions`: absolute path to a Markdown file inside an approved repository.
- `provider`, `model`, `permission`: execution settings for this task type. Providers
  are `codex` and `claude`; `dangerFullAccess` normalizes to `danger-full-access`, and
  `bypassPermission` to `bypassPermissions`. Permissions must match the provider.
- `parameters`: map from parameter name to a value schema, with optional boolean
  `required` (default false). Inputs are always objects and unknown keys are rejected.
  Use `{}` for no parameters. Constraints such as `minLength`, `enum`, `minimum` and
  `items` use JSON Schema syntax; external schema URLs are disabled.

`default_max_workers`, `default_lease_secs`, `default_task_limit_secs`, and
`default_task_limit_tokens` populate controller creation. If omitted, defaults are
1 worker, 60 seconds, 3,600 seconds and 2,000,000 reported tokens. Operators can
change all four per controller. Values must be positive, leases at least 15 seconds,
and task time at least the lease duration. Token limits depend on provider reporting
and can overshoot; they are not hard billing caps. Reported cost remains recorded
but there is no configured cost cutoff.

## Rendering, file contracts and loading

Task definitions declare `inputs` and `outputs` as arrays of relative path strings.
Markdown and paths support literal `{{parameter}}` substitution after schema validation.
Missing placeholders and template expressions are rejected; parameter values are never
executed or recursively expanded. `upstream:<path>` in inputs explicitly requires the
immediate predecessor's accepted artifact. Plain paths read the working directory.
Outputs cannot use upstream prefixes or escape the working directory.

Before launch the controller validates all declared inputs and records signatures.
Before accepting completed results it checks all required outputs and rejects missing
files, escaping paths, non-regular files or stale files unchanged since preparation.
It automatically archives declared outputs in addition to optional worker-submitted
artifacts. Identical-content regeneration is allowed when the file is rewritten;
failed/blocked submissions can provide partial evidence. File checks do not establish
semantic correctness. Use distinct workflow-specific paths for concurrent tasks that
otherwise write the same shared checkout paths.

`python -m agent_manager.orchestration render|enqueue --profile NAME --file BATCH.json`
provides operator tools. JSON stdin also works. The UI's Load tasks dialog accepts a
file or pasted JSON, displays rendered assignments/notes, and loads after preview.
HTTP routes are POST `/api/task-queues/{title}/render` and `/enqueue`; these do not
require a running controller. Worker MCP capabilities cannot enqueue or approve tasks.

A batch supplies batch_key, workflow_id and 1–50 tasks, each with key, task_type,
parameters and optional depends_on (earlier batch key), depends_on_id (existing task),
human_review_required (default true), max_attempts (default 3), task_order and priority.
Loading is transactional and repeat-safe: the first task's stable producer key locks
batch identity, and request hashes reject changed content under the same batch key.
Repeated loads return existing IDs without resetting task state. UI loads supply a
preview_hash to reject a changed preview; previews do not freeze future definitions.
Required local files can still be supplied before execution; unresolved predecessor
files are identified explicitly. Enqueueing does not launch a stopped controller.

## Tables and producer contract

Names are static: `am_schema_version`, `am_tasks`, `am_task_attempts`, `am_work_log`.
Internal integration tests substitute a temporary prefix; it is not a deployment
option. All timestamps use UTC with microsecond precision.

A producer can insert tasks before a controller exists:

```sql
INSERT INTO am_tasks
  (queue_id, workflow_id, task_type, task_order, parameters,
   human_review_required, max_attempts)
VALUES
  ('sample', 'pilot-001', 'report', 10,
   JSON_OBJECT('topic', 'the project README'), TRUE, 3);
```

`workflow_id` groups a run and its dependencies, allowing repeated uses of the same
task types with different parameters. Each task row has its own `id`. Direct SQL producers
must make enqueueing repeat-safe using stable IDs or a producer-owned transaction;
`workflow_id` does not itself prevent duplicate inserts.

`task_order` and `priority` influence scheduling, not dependencies. `depends_on`
is a single predecessor in the same queue/workflow, which must be completed and
reviewed before the successor becomes eligible. Cycles and cross-workflow references
block work. After the first claim, task inputs and dependencies are immutable by
protocol. For changed inputs or a new definition after execution, enqueue a new task
row/workflow revision. Direct SQL modification can violate these guarantees.

## Workspace mode

`use_isolated_workspace` is a profile boolean, defaulting to `true` when omitted.
The selected mode is saved in each attempt snapshot and retained on retry.

- `true`: export committed Git HEAD into a private attempt workspace, overlay the
  snapshotted instructions, and retain the source archive for replay.
- `false`: work directly in the configured repository containing the task instructions.
  Uncommitted, untracked and ignored files are visible; edits affect that checkout.
  No Git commit or source export is required, and preparation never cleans or overlays
  the checkout. The recorded Git revision, when available, is informational only.

In shared mode, instructions/configuration/parameters/limits are still snapshotted,
while retries read the current filesystem. The saved base prompt is retained; an
attempt-specific context suffix identifies its evidence directory. Accepted predecessor
files are copied to `queue-work/<queue_id>/attempts/<attempt-id>/.queue-inputs/` under
the state directory, outside the checkout. Its absolute path is given in the prompt
and added to the worker's accessible directories. Nothing is copied into a shared
checkout's `.queue-inputs` directory. Submit artifact paths relative to the checkout;
accepted bytes are still archived by hash. Keep shared-mode writers serialized across
controllers; per-controller max_workers does not provide a checkout-wide lock.

The following source-export guarantees apply to isolated mode.

## Snapshots, repositories and retries

The task resolver rereads the selected profile's task definitions for a new task,
validates parameters, reads the instruction file and resolves the containing approved
repository's current Git HEAD. The repository must have a commit. Instructions are
captured from the current file, including uncommitted edits; repository code comes
from the committed HEAD tree. Commit code changes before starting tasks that need them.
Only the repository containing the instructions is exported into the attempt workspace;
the instruction file is overlaid with its exact snapshotted contents.

`am_task_attempts.input_snapshot` stores the resolved task definition (including
provider/model/normalized permission and parameter schema), exact instruction text,
definition hash, actual parameters, effective limits, repository alias/commit/archive
hash, accepted predecessor result, and final worker prompt. Limits are stored at claim;
full preparation is stored before provider launch. A failure before preparation has
no executed instruction snapshot; unblocking may resolve the corrected definition.

Retries, including explicit retries of executed tasks, reuse the preceding saved
snapshot. They get a new attempt ID, conversation and workspace. Profile edits, changed Markdown or controller limit changes cannot alter the saved
instructions/configuration. Isolated mode also retains source bytes; shared mode
uses the live filesystem and updates the evidence-directory suffix per attempt. A restarted controller resumes the same prepared attempt without resending an
already-reserved prompt. New task rows use current definitions. Controller default
changes affect new controllers; existing controller settings persist independently.

Storage is derived automatically from:

```text
${AGENT_MANAGER_STATE_DIR:-/var/lib/agent-manager}/queue-work/<queue_id>/
  controllers/<controller-id>.json  # local pause/limits across restart
  attempts/<attempt-id>/           # writable isolated source export
  sources/<sha256>                 # retained Git archive, independent of Git GC
  artifacts/<sha256>               # accepted output bytes
  cancelled-attempts/<attempt-id>  # cancellation tombstones
```

Persist that state directory, including its capability secret. There is no
`workspace_root` profile setting. Archives are hash-checked on replay; sources are
not writable shared Git checkouts. Links, special files, traversal, more than 100,000
entries or archives over 1 GiB are rejected. Instructions are bounded to 2 MiB. No
checkout hooks, filters or setup commands run during export.

Accepted predecessor artifacts are verified and copied under
`.queue-inputs/<original-relative-path>`. Missing/corrupt evidence blocks launch.
The repository must not use that reserved directory. Workers must submit changed
source files or a patch as artifacts; unsubmitted edits are not accepted deliverables.

## Dispatch, limits and recovery

Task-row claims use `FOR UPDATE SKIP LOCKED`; claim and attempt insertion commit
together. A process lock prevents concurrent loops for the same controller identity.
Each controller serializes its claims and counts its live attempts before claiming.
Claimed, running and submitted attempts hold slots until execution ends or cancellation
is confirmed. A scan may temporarily skip locked work and try again next cycle.

Worker limits and pause state are per controller. Two controllers at limit 2 may run
4 workers total; atomic claims prevent duplicate assignment. Raising capacity permits
more work; lowering it lets existing workers finish. Pause prevents new claims.
The right panel shows pool-wide eligible backlog, active tasks/workers owned by this
controller, and editable worker/lease/time/token settings. Changes to attempt limits
apply to new tasks; retries retain their original snapshot limits. Unavailable counts
are displayed as unavailable, not zero.

Launch uses attempt ID as its idempotency key. The backend persists conversation
reservation and an execution-input hash before starting a provider. Repeated identical
requests return that conversation; changed prompts or execution settings conflict.
A crash in the ambiguous reservation/start window fails the attempt rather than
resending possibly executed work. Cancellation persists a tombstone, including when
launch has not arrived yet.

Expired attempts retain their state until the backend confirms the original worker
has stopped. Another controller in the same backend can reconcile expired attempts.
A worker ending without a valid structured result fails, with bounded retry/backoff;
`ready` alone is not success. Delay is `min(300, 5 * attempt_count)` seconds. Default
attempt budget is 3. Scheduling and budget checks require a live controller; process
restart after an unexpected exit remains a deployment responsibility.

## Results, review and retention

Workers receive attempt-scoped `queue_progress` and `queue_submit_result` MCP tools.
Provider-native tools may still be present; host-command MCP is not automatically
added. Database/configuration variables are removed from worker environments.
Capabilities are HMAC-derived from the durable backend secret and random attempt ID.
The server checks capability, active state and lease transactionally.

- Progress: concise summary, at most 4,000 bytes.
- Final result: `completed`, `blocked` or `failed`, summary at most 16,000 bytes,
  optional relative artifact paths and blockers. Chat alone cannot complete a task.
- Exact repeated results are idempotent; conflicting submissions are rejected.
- Submissions are bounded to 1 MiB. Artifacts are regular contained files: at most
  50 files, 16 MiB each, 64 MiB total. Stored SHA-256 bytes remain immutable.

Completion either completes the task or enters `awaiting_review`. Human approval
names the exact latest attempt and promotes its result. Rejection fails the task;
bounded explicit retry is allowed. Review waits consume no worker slot.
Work logs are append-only reports/transitions with SQL IDs as replay cursors.

Drain pauses only this controller and waits for its workers. Deleting a controller
stops its workers but retains SQL history, artifacts and attempt conversations. If
already stopped, outstanding leases are reconciled after expiry by a later controller.
Managed attempts cannot bypass accounting through direct send/abort/delete/reparent.

The existing operator UI, profile files and state directory are trusted. Attempt
capabilities are not a sandbox for workers with unrestricted access to that same
filesystem/network. Consumer operations outside the workspace must be authorized
and idempotent. Retain SQL, state, sources and artifacts together; there is no automatic
history garbage collection or cross-controller provider-account quota coordinator.

## Validation

`go test -race ./...`, Python `pytest`, and `node --test tests/*.cjs` cover the scheduler,
adapters and UI. `AM_TEST_MYSQL_DSN` enables integration tests in temporary namespaces
that clean up afterward. `tests/browser_task_queue_smoke.py` uses Chromium and mocked
APIs to verify rendering and controls without launching workers. A real provider pilot
remains a manual deployment step.

### Human approval

When `human_review_required` is true, successful work enters `awaiting_review`.
In the queue’s Tasks view, expand the task, inspect its result and artifact links,
then click **approve** or **reject**. Approval accepts that attempt and releases
dependent tasks; rejection marks it failed. This flag does not schedule an LLM
review task. The JSON/API field and SQL column are both named `human_review_required`.
