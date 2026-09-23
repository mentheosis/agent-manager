# Worker/reviewer execution

A task may opt into iterative review through its profile definition:

```json
"reviewer": {
  "instructions": "task_definitions/probe_reviewer.md",
  "provider": "codex",
  "model": "gpt-6-astra",
  "permission": "read-only"
}
```

Reviewer instruction paths follow the same repository/base_path convention as task
instructions. Their contents and model settings are pinned in the attempt snapshot.
Codex reviewers use read-only; Claude reviewers use plan permissions. Reviewers do
not receive the replay profile. Task-specific acceptance criteria belong in the
repository instructions, never in agent-manager code.

The existing Go scheduler owns claims, leases, parallel task capacity, budgets,
cancellation, and finalization. attempt_runner.go runs a durable state machine within each
attempt: worker proposal → reviewer → continue, accept, or escalate. There is no
nested team leader or second scheduling process. Shared orchestration client, MCP
transport, backend conversation lifecycle, registry, and event presentation are
reused. Teams retain their existing leader-driven behavior.

Each role turn has a deterministic ID, a persisted prompt, its own capability,
and one immutable submission in am_task_rounds. Rounds do not increment
attempt_count. An attempt keeps one worker conversation and resumes its retained
provider session after each review. Its next prompt contains only the latest
reviewer feedback; the initial assignment is not sent again. Each reviewer starts
in a new conversation with fresh context.

Before resuming the worker, the backend rotates its round capability by restarting
the runtime with the retained provider session. Usage is measured from the new
round's history offset to avoid counting earlier rounds twice. Persisted launch
reservations prevent duplicate feedback delivery after a controller restart. If
the original worker session cannot be resumed, the attempt blocks instead of
silently creating a replacement. An attempt recovered directly into review without
a worker session sends the initial assignment once when its first worker starts.
queue_read_history reads intact events in pages of 50 within the same attempt. It
does not truncate individual messages.

Worker queue_submit_result proposes completed, blocked, or failed. All proposals are
reviewed, including proposed blockers. Required output checks run for completed
proposals; submitted artifacts are copied by content hash. Reviewers receive copies
of those artifacts under the attempt evidence directory. Only the finally accepted
proposal is released downstream. Reviewer queue_submit_review requires evidence:

- continue: specific next_steps;
- accept: an evidenced completed proposal;
- escalate: an external_dependency describing the required human action.

The controller rejects wrong-role tools, altered duplicate submissions, and stale
turn submissions. It stops a submitted conversation before launching the next role.
Launch reservations and stable IDs prevent duplicate dispatch after restart. Expired
leases require acknowledged cancellation before failure/retry releases capacity.
Cancellation covers every turn. Active attempts retain one parallel-task slot,
regardless of the number of completed turns. Human approval remains a separate gate.

Time/token limits cover the whole attempt. Reported token usage is summed across
turns. At 90% of time or reported tokens, a working turn is stopped and its available
output checkpoint is sent to review. Hard limits still stop execution. Reporting
can lag, so token limits are not an exact provider-side spending cap. Round-limit,
budget-limit, and repeated identical review feedback leave explicit resumable
blocked reasons, not claims of impossibility. Retrying retains pinned instructions
and brings forward the prior attempt's latest proposal artifacts and findings.

The Tasks table shows Working/Reviewing and round number. Expanded details include
round decisions and conversation references. Role conversations appear beneath the
queue controller; activity records round starts and submissions.

## Initial setup and verification

Reapply schema/001.sql before starting the rebuilt controller. It adds
am_task_rounds without deleting existing rows. This is still the initial schema,
not a versioned migration. Any manual reset must delete/drop am_task_rounds before
am_task_attempts because of its foreign key. Existing pinned attempts keep their
original execution settings; enqueue a fresh task to enable newly configured review.

Run Go tests with AM_TEST_MYSQL_DSN against a test-capable MySQL database. Integration
tests use random internal table prefixes and clean up their own tables. Run Python
API tests and the existing browser-component tests. For manual testing, use a small
neutral task whose first proposal deliberately leaves a testable gap, verify a
continue round, restart the controller, and finish through the human approval gate.
Also test cancellation and a deliberately small budget before running chain work.

Specialist delegation is deferred until the pair is validated. There is no new
LLM authority to enqueue tasks, approve human gates, or increase limits.

## Reviewer runtime and interrupted reviews

Reviewers use `queue_read_file` to enumerate/read only the declared task inputs and
archived proposal files. Reads are paginated and do not invoke a shell. This works
in containers where the provider's shell sandbox cannot create user namespaces.
The reviewer remains read-only. Its queue server exposes only file/history reads,
progress, and review submission; host replay and worker result submission are absent.
Codex launch configuration marks the queue server required and explicitly approves
these scoped tools using per-tool approval settings. No global approval/sandbox
bypass is enabled. See the official [MCP configuration reference](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).

A reviewer ending without a structured decision is an execution infrastructure
blocker, not a failed research task or an instruction to repeat the worker. Operator
retry renders the latest definition and starts a fresh worker, without inheriting the
old proposal as a submission. A worker continuation
only follows an explicit continue decision. Submitted conversations have up to ten
seconds to deliver their final events before the controller stops them and publishes
ready status. Conversation Cancel routes to the queue controller, cancelling the
whole current task attempt; stale attempt IDs cannot cancel newer work.

## Live controller limits and blocked reasons

Token, execution-time, and lease settings belong to the controller. They are not
stored in task snapshots. Saving a token/time change affects running worker/reviewer
attempts on the next scheduler cycle, including reductions. Lease renewals use the
current duration; expired ownership is never silently revived. Retries use the current
controller settings while retaining task instructions and evidence. Time elapsed is
still measured from the attempt start; saving a limit does not reset its clock.
A limit change does not automatically restart a terminal blocked task.

Blocked/failed rows show a short reason badge and a highlighted explanation in their
expanded details. Attempt-count exhaustion is a separate per-task retry restriction;
the Retry button remains visible but disabled with its reason when no attempts remain.

Controller budget interruptions are recorded with `origin: controller_checkpoint`.
They preserve available files but are not worker submissions or validated reports.
The remaining 10% permits one checkpoint review. A continue decision cannot launch
another worker while the current controller limits remain at or above the worker
cutoff; the attempt blocks with the limiting resource and saved feedback. Raising
a live controller limit before that decision allows continuation under the new
limit. Review history accepts round IDs or conversation IDs, restricted to the
same attempt.

### Resume versus Retry

For a worker/reviewer attempt blocked by a controller time or token limit, **Resume**
continues the same attempt and worker conversation after limits are increased.
It keeps the attempt count, original start timestamp, files, review history, and
cumulative token usage. Time spent blocked is excluded from the running-time
budget using persisted `resource_usage.paused_micros`; no schema change is needed.
The controller must be running with a free worker slot, and the original worker
session must still exist. Resume rejects unchanged/insufficient budgets and
missing sessions instead of creating replacements.

Resume opens a new worker round with the latest completed review's feedback, or
a short continuation instruction if no review completed. Interrupted rounds stay
in the history; they are never represented as completed worker/reviewer decisions.
The next reviewer uses a fresh conversation. Old round capabilities remain cancelled.
**Retry** starts a new attempt and remains subject to the task's attempt limit.
Resume currently applies to worker/reviewer attempts; other failures use Retry.

Review rounds are a live controller limit, configured by profile
`default_review_round_limit` (default 8) and UI **Review round limit**. The former
per-task `reviewer.max_rounds` value is ignored, including in old attempt snapshots.
Saving the controller limit takes effect on the next decision. A round-limit
block can Resume the same attempt once the limit exceeds its latest round number;
token/time and worker-capacity checks still apply.

### Fresh Retry assignments

Retry creates a new attempt with a freshly resolved current profile: worker/reviewer
instructions, models, permissions, repository/workspace mode, inputs and outputs.
Stored task type and parameters remain the task identity; incompatible current schemas
block preparation visibly. Previous attempts and their snapshots remain immutable.
Retry starts a fresh worker conversation and never imports an old proposal as the new
attempt's submission or skips directly to review. Shared-workspace files are preserved,
so the new instructions may explicitly reuse them as evidence. Controller limits stay
live. Resume and recovery within an already prepared attempt retain its snapshot.

## Human conversation prompts

The normal conversation input is enabled for queue workers and reviewers. Before
sending a prompt, the backend asks the controller to classify the conversation.

- A current worker in a blocked, failed, or awaiting-human-review task resumes the
  same attempt at a new worker turn, using only the user's message.
- The latest reviewer can similarly resume its retained conversation in a new
  review turn. It evaluates the most recent worker proposal and its immutable
  evidence. Its decision can continue work, accept completion, or escalate again.
- Superseded attempts, older reviewers, and completed tasks allow
  independent conversation without changing task state. Such conversations have
  queue tools removed and submissions fenced. The UI identifies this mode.
- Current active work cannot be interrupted through this path. Pending
  continuations reject duplicate sends. A stopped/paused controller, exhausted
  limits, missing retained session, or full capacity returns an actionable error;
  the prompt draft remains in the input.

Resume preserves the attempt identity, snapshot, cumulative usage, and provider
session. It restores the lease, excludes stopped time, clears the old attempt
submission, rotates the round capability, and records human feedback in the work
log. A resumed worker is followed by a fresh reviewer. A manually resumed reviewer
retains its own conversation; automatic reviews still start fresh conversations.
No schema migration is needed: continuation references use round resource metadata.

### Stopping a conversation

The conversation Cancel/Stop control pauses the current attempt. It stops and
fences all attempt execution, releases capacity, and stores a resumable blocked
state with the visible reason “Paused by operator”. A normal prompt to the current
worker or reviewer resumes the same attempt; no automatic retry occurs while
paused. Queue-level task Cancel remains an explicit cancellation. A subsequent
user prompt can also reopen the latest cancelled attempt, including conversations
cancelled by the old thread-stop behavior. Older attempts remain detached.
Controller ticks and control operations are serialized so a late tick cannot
overwrite a pause or launch another review while it is being applied.

### Single-worker continuations and prompt durability

Tasks without reviewers can resume through the same conversation input. On the
first continuation, the controller preserves the original execution as round 1
and creates a new worker round, retaining attempt identity, usage, snapshot and
provider session. Results finish directly without introducing a reviewer.
Incoming queue prompts are recorded as user_prompt_received before routing or
runtime startup. Configuration failures leave that receipt recoverable and emit
an explicit error rather than silently ending the runtime.
