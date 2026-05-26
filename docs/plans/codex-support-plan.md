# Plan: Add Codex Support Alongside Claude

This repo currently treats every normal agent as a Claude Code SDK session. To support Codex cleanly, the main refactor should separate three ideas that are currently blended together:

- Provider: which coding agent runtime backs an instance (`claude`, `codex`).
- Kind: what role the instance plays in agent-manager (`agent`, `loop`/team).
- Runtime options: model, permissions/sandbox policy, extra writable directories, provider-specific config files, auth state, and resume/session behavior.

The goal is to keep the FastAPI API, persistence, WebSocket event stream, static UI, and Go orchestrator provider-neutral while isolating Claude and Codex process/SDK details in provider adapters.

## Current Codebase Understanding

### Python service

The FastAPI app is built in `src/agent_manager/server.py`. It owns:

- instance CRUD and summaries,
- model listing,
- auth/login endpoints,
- WebSocket event delivery,
- orchestration/team endpoints,
- file/diff/rules/plans/memory endpoints.

`src/agent_manager/instance.py` is the core runtime object. It currently does two jobs:

- generic instance lifecycle: inbox queue, status, pub/sub, history, event sequence numbers, persistence hooks, abort/reload/stop;
- Claude-specific runtime: constructs `ClaudeAgentOptions`, starts `ClaudeSDKClient`, sends prompts, builds Claude multimodal payloads, translates Claude SDK message classes into UI events, extracts Claude session/model metadata.

`src/agent_manager/state.py` is the in-memory registry. It creates and reloads `Instance` objects, persists metadata changes, wires event hooks, and stores team relationships. `instance_type` currently has values `"claude"` and `"loop"`, which makes `"claude"` mean "normal agent" rather than provider. That will become confusing as soon as a normal agent can be backed by Codex.

`src/agent_manager/persistence.py` persists ordered instance records to `instances.json` and event history to `events/{title}.jsonl`. The persisted record has Claude-specific fields (`permission_mode`, `add_dirs`, `session_id`) and an overloaded `instance_type`.

`src/agent_manager/auth.py` is fully Claude-specific: it checks Claude auth status and runs `claude auth login` inside a pty.

`src/agent_manager/files.py` is mostly generic except memory support, which encodes paths using Claude's `~/.claude/projects/.../memory` layout.

### Frontend

The static UI expects a single provider:

- `am-new-dialog.js` creates "Single Claude instance", hard-codes Claude permission modes, Claude model examples, and merges YAML permissions into `.claude/settings.json`.
- `am-permissions-panel.js` exposes model, Claude permission mode, and allowed directories.
- `am-auth-banner.js` and `am-login-dialog.js` assume one global Claude auth status and run the Claude auth login flow.
- rules/plans/memory views expose Claude-specific files like `CLAUDE.md`, `.claude/settings.json`, `.claude/plans`, and Claude memory.
- `streams.js` and `am-terminal-pane.js` are reasonably provider-neutral already because they consume normalized event types such as `assistant_text`, `tool_use`, `tool_result`, `result`, `system_init`, `error`, `aborted`, and `status`.

### Go orchestrator

The Go orchestrator talks to the Python API instead of the provider directly. That is good for Codex support. It relies on:

- `/api/instances`,
- `/api/instances/{title}/children`,
- `/api/instances/{title}/send`,
- `/api/instances/{title}/history`,
- status values (`creating`, `ready`, `running`, `error`),
- normalized event types, especially `assistant_text`, `tool_use`, `tool_result`, and `thinking`.

As long as Codex instances publish the same normalized event stream and status lifecycle, the orchestrator should not need to know whether a child is Claude or Codex.

### Docker/runtime

The Dockerfile installs `@anthropic-ai/claude-code` and the Python `claude-agent-sdk`. Compose persists `claude-auth` to `/app/.claude` and `agent-manager-state` to `/var/lib/agent-manager`. Codex support will need its own CLI/package and persistent auth/config volume, probably `/app/.codex`.

## Codex Runtime Facts To Account For

Local `codex-cli 0.133.0` is installed in this environment. Current official Codex docs expose four relevant automation surfaces:

1. `codex exec --json`: stable, non-interactive, process-per-run JSONL automation.
2. Codex SDK: official TypeScript package `@openai/codex-sdk`; Python SDK exists but is experimental and currently described as controlling the local app-server from a local checkout of the open-source Codex repo.
3. `codex app-server`: JSON-RPC protocol used by richer Codex clients, with conversation history, approvals, streamed events, and thread/turn lifecycle. The docs mark WebSocket transport as experimental/unsupported; stdio JSONL is the default transport.
4. `codex mcp-server`: exposes Codex as an MCP server with tools for starting and continuing Codex sessions from another MCP client.

For this Python/FastAPI repo, the first implementation should probably use `codex exec --json` rather than the SDK:

- It is documented as stable and intended for scripted/CI-style use.
- It avoids adding a Node sidecar just to use the TypeScript SDK.
- It avoids depending on the experimental Python SDK or maintaining a local checkout of Codex.
- It maps cleanly to agent-manager's existing "send prompt, stream events, persist normalized history" model.

The adapter boundary should still be strong enough to replace `codex exec --json` later with app-server or a packaged Python SDK if we need richer features like in-flight steering, approval prompts, or first-class stored thread reads.

Known `codex exec` options we need:

- `codex exec` runs non-interactively.
- `codex exec --json` prints JSONL events.
- `codex exec -C/--cd <DIR>` sets the working root.
- `codex exec --add-dir <DIR>` adds writable directories.
- `codex exec -m/--model <MODEL>` sets the model.
- `codex exec -s/--sandbox read-only|workspace-write|danger-full-access` sets sandbox policy.
- `codex exec -a/--ask-for-approval untrusted|on-request|never` sets approval behavior.
- `codex exec resume ...` can resume a prior session.
- `codex login` supports device auth and stdin API/access-token login modes.
- `codex mcp` manages external MCP servers.

The `--json` output is the critical contract. Official docs say it emits newline-delimited JSON events, including `thread.started`, `turn.started`, `turn.completed`, `turn.failed`, `item.*`, and `error`. Item types include agent messages, reasoning, command executions, file changes, MCP tool calls, web searches, and plan updates.

For agent-manager, Codex should use the same high-level continuity model as Claude:

- agent-manager persists a provider-neutral `session_id` on the instance record;
- the provider owns the full transcript/session storage under its auth/config home;
- agent-manager stores normalized UI events for replay and orchestration, not as the source of truth for model-visible conversation context;
- the first Codex prompt creates a new persisted Codex thread/session;
- later prompts resume that thread/session by id instead of replaying the whole transcript into the prompt.

The expected Codex command flow is:

```text
# First prompt for an agent-manager instance
codex exec --json -C <cwd> <prompt>

# Follow-up prompts after capturing the emitted thread/session id
codex exec resume <session_id> --json <prompt>
```

This should make Codex behavior close to the current Claude behavior: process lifetime is different, but conversation continuity is still delegated to the provider runtime.

Open questions to resolve with real authenticated fixture runs before implementation:

- Which JSONL event contains the stable thread/session id for `codex exec --json`?
- Is the emitted `thread_id` exactly the value accepted by `codex exec resume [SESSION_ID]`?
- Does `codex exec resume <SESSION_ID> --json <prompt>` emit the same event shapes as a fresh run?
- Does the JSONL stream include enough detail to distinguish command stdout/stderr, file edits, MCP calls, and plan updates?
- How does `codex exec --json` represent command failures, sandbox denials, approval-required events, and nonzero process exits?
- What session files appear under `$CODEX_HOME`, and should agent-manager ever inspect them directly, or only trust JSONL ids?
- What does image input look like in JSONL when using `--image`?
- Can we use `--output-last-message` as a fallback for final assistant text if the JSONL stream changes?

## Proposed Architecture

### 1. Introduce provider-neutral models

Add a small set of shared types, for example:

- `AgentProviderName = Literal["claude", "codex"]`
- `InstanceKind = Literal["agent", "loop"]`
- `AgentConfig`
  - `provider`
  - `cwd`
  - `model`
  - `add_dirs`
  - `permission` or provider-specific runtime settings
  - `session_id`
  - `images` support flag/input payload
- `AgentEvent = dict[str, Any]`
- `AgentInput`
  - `text`
  - optional images

Persist `provider` separately from kind. Migrate:

- old `instance_type == "claude"` -> `kind = "agent"`, `provider = "claude"`;
- old `instance_type == "loop"` -> `kind = "loop"`, `provider` may be `claude` for the loop backing session until team leaders can be created as Codex.

For API compatibility, `_summary()` can keep returning `instance_type` during a transition, but new code should use `kind` and `provider`.

### 2. Split `Instance` lifecycle from provider runtime

Keep `Instance` as the owner of:

- title/path/status/metadata,
- inbox queue,
- history and subscribers,
- sequence numbers,
- persistence hooks,
- status publication,
- abort/reload orchestration.

Move provider-specific behavior into adapters:

```python
class AgentRuntime(Protocol):
    provider: str

    async def start(self) -> None: ...
    async def run_turn(self, message: AgentInput) -> AsyncIterator[Event]: ...
    async def abort(self) -> None: ...
    async def reload(self, config: AgentConfig) -> None: ...
    async def close(self) -> None: ...
```

The `Instance._run()` loop should become:

1. create runtime from `ProviderRegistry`;
2. publish `ready`;
3. wait for inbox message;
4. publish `user_prompt`;
5. publish `running`;
6. iterate runtime events and publish them;
7. publish `ready` or `error`.

Claude and Codex adapters should be the only modules importing their SDK/CLI-specific packages or parsing provider-specific event formats.

Suggested files:

- `src/agent_manager/providers/base.py`
- `src/agent_manager/providers/registry.py`
- `src/agent_manager/providers/claude.py`
- `src/agent_manager/providers/codex.py`
- `src/agent_manager/providers/auth.py` or provider-specific auth modules

### 3. Claude adapter

Move the current Claude code from `Instance` into `providers/claude.py`:

- build `ClaudeAgentOptions`;
- manage `ClaudeSDKClient`;
- handle `resume`, `permission_mode`, `add_dirs`, `model`, Docker MCP config;
- build multimodal Claude content;
- translate `AssistantMessage`, `UserMessage`, `ResultMessage`, and `SystemMessage` to normalized events;
- extract `session_id` and resolved model from `system_init`/`result`.

This should initially be a near move-only refactor so behavior remains stable.

### 4. Codex adapter

Start with a Codex subprocess adapter based on `codex exec --json`. Treat this as a provider implementation detail behind `AgentRuntime`, not as an API exposed to the rest of agent-manager.

For each turn, spawn something close to:

```text
codex exec --json -C <cwd> [--model <model>] [--add-dir <dir>...] [--sandbox <mode>] [--ask-for-approval <policy>] [--image <file>...] <prompt>
```

For resumed turns, use the documented/current resume command once verified against JSONL output:

```text
codex exec resume <thread_id_or_session_id> --json <prompt>
```

The adapter should:

- start one subprocess per turn, not one permanent process per instance;
- choose fresh `codex exec` only when the instance has no persisted `session_id`;
- choose `codex exec resume <session_id>` for follow-up prompts;
- parse stdout as JSONL into provider-specific raw events while concurrently reading stderr;
- translate those into the existing normalized UI events;
- publish a `system_init` equivalent with provider/model/runtime metadata when a thread or turn starts;
- capture the Codex thread/session id from JSONL and persist it in `Instance.session_id`;
- avoid inspecting `$CODEX_HOME` session files unless fixture work proves there is no stable JSONL id;
- treat stderr as progress diagnostics by default, only publishing it as an `error` when the process exits unsuccessfully or JSON parsing fails;
- map process exit to `result` with `is_error`, duration, and available usage metadata;
- support abort by terminating the child process and publishing `aborted`;
- handle images by writing temporary files for base64 uploads and passing `--image` arguments, then deleting temp files after the process exits.

Codex event translation should target the existing event contract, not the UI directly. Expected mapping to verify with fixtures:

- `thread.started` -> store `session_id`/thread id and publish `system_init` metadata if this is the first event.
- `turn.started` -> optional `provider_turn_started` or no-op after status is already `running`.
- `item.started` for `command_execution`, MCP, web search, or file-change items -> `tool_use` with a synthesized or source `id`.
- agent message item deltas or completed agent messages -> `assistant_text`.
- reasoning summary deltas or completed reasoning items -> `thinking`.
- command output deltas and completed command executions -> `tool_result`.
- file change items -> either `tool_use`/`tool_result` for UI parity or a new normalized `file_change` event if the UI needs richer diff display later.
- `turn.completed` -> `result` with usage.
- `turn.failed` or `error` -> `error` plus final `result.is_error = true`.

If Codex JSONL does not produce stable tool ids, synthesize per-turn ids such as `codex-{turn_seq}-{item_id}` so nested tool results keep working in `am-terminal-pane.js`.

Keep a small `providers/codex_events.py` translator with table-driven tests from recorded JSONL fixtures. Do not spread JSON event parsing across `Instance`, `server.py`, or frontend code.

### 5. Provider capabilities endpoint

Replace the single `/api/models` assumption with provider-aware capabilities:

```text
GET /api/providers
GET /api/providers/{provider}/models
GET /api/providers/{provider}/auth/status
POST /api/providers/{provider}/auth/login
```

Capabilities should describe UI controls:

- supports images,
- supports model selection,
- supports add_dirs,
- permission/sandbox controls and allowed values,
- rules/plans/memory files,
- auth mode (`pty-login`, `api-key`, `device-auth`, `none`),
- supports persistent sessions/resume.

For compatibility, `/api/models` can temporarily return Claude models or accept `?provider=claude|codex`.

Model listing should also be provider-specific:

- Claude: current Anthropic fetch/fallback behavior.
- Codex: either a static recommended model list derived from current Codex docs/config or the OpenAI model API if `OPENAI_API_KEY` is available. Avoid mixing Anthropic and OpenAI IDs in one dropdown without a provider selector.

### 6. Auth refactor

Generalize `AuthRegistry` into provider auth sessions:

- Claude provider:
  - credentials path: `~/.claude/.credentials.json`;
  - login command: `claude auth login`;
  - preserve existing pty streaming UI.
- Codex provider:
  - credentials/config path: `$CODEX_HOME` or `~/.codex`;
  - login status: prefer `codex login status` when available rather than only checking files;
  - login command: `codex login --device-auth` for interactive browser auth, plus a non-interactive API-key path if the UI later supports secret entry;
  - persist `/app/.codex` in Docker.

The frontend banner should show provider-specific auth health. A simple first version can show unauthenticated if any configured provider for existing instances is unauthed, and the new-instance dialog can show the selected provider's auth state.

### 7. Provider-specific files

Keep file tabs provider-aware:

- Claude rules: `CLAUDE.md`, `.claude/settings.json`, `.claude/settings.local.json`, `.mcp.json`.
- Codex rules/config: likely `AGENTS.md`, `.codex/config.toml` or project/user Codex config as supported by the CLI, plus `.mcp.json` if applicable.
- Plans: Claude currently uses `.claude/plans`; Codex may not have an equivalent. Either hide the Plans tab for Codex until a target layout is defined, or define agent-manager-owned plans under `.agent-manager/plans`.
- Memory: Claude helper currently relies on Claude's encoded project path. Do not reuse it for Codex. Either add a Codex provider memory helper if Codex exposes stable memory files, or mark memory unsupported in provider capabilities.

Avoid writing Codex settings through the existing `settings_json` field. Replace it with provider-specific config input or a generic `provider_options` payload validated by the provider.

### 8. API and persistence changes

Recommended persisted record shape:

```json
{
  "title": "example",
  "kind": "agent",
  "provider": "claude",
  "path": "/repo",
  "model": "claude-sonnet-...",
  "session_id": "...",
  "created_at": "...",
  "runtime_options": {
    "permission_mode": "acceptEdits",
    "add_dirs": []
  },
  "parent": null,
  "children": [],
  "agent_preset": null,
  "task": null,
  "folder": null
}
```

The `session_id` field should remain provider-neutral. For Claude it is the Claude SDK/CLI session id. For Codex it should be the id used by `codex exec resume`, expected to come from the JSONL thread/session start event after Phase 0 verification. Do not introduce a second field such as `codex_thread_id` unless research proves Codex needs multiple ids.

During migration, tolerate both old and new fields:

- read `instance_type` if `kind` is absent;
- read `permission_mode` and `add_dirs` into `runtime_options` if absent;
- keep writing old fields for one release if the UI is not migrated at the same time.

Creation should accept:

```json
{
  "name": "agent",
  "provider": "codex",
  "path": "/repo",
  "model": "<codex-model-id>",
  "runtime_options": {
    "sandbox": "workspace-write",
    "approval_policy": "never",
    "add_dirs": []
  }
}
```

The current `PermissionsBody` should become a generic `RuntimeOptionsBody`, while the UI labels should be driven by provider capabilities.

### 9. Orchestrator compatibility

Keep the Go orchestrator provider-neutral. It should continue to:

- send prompts via `/send`;
- read normalized history;
- monitor status;
- ignore provider-specific fields unless it wants to display them.

Potential improvement: include `provider` in `InstanceInfo` so the leader can understand its team composition. This is informational only.

The leader/orchestrator agent can be Claude, Codex, or mixed once provider selection is supported for team YAML. Team YAML should gain optional `provider` fields:

```yaml
title: my-team
path: /repo
leader:
  provider: claude
  model: claude-sonnet-...
agents:
  - name: coder-codex
    provider: codex
    model: <codex-model-id>
    path: /repo
```

Default provider should remain Claude for backward compatibility.

## Implementation Phases

Use these checklists to track implementation once the refactor starts. Keep them in this doc until the work is complete.

### Phase 0: Codex Integration Research

- [ ] Authenticate Codex in the same container/user model we use for Claude.
- [ ] Run `codex exec --json "say hello"` and save a sanitized fixture.
- [ ] Run `codex exec --json -C <repo> --sandbox workspace-write "inspect this repo"` and save a sanitized fixture.
- [ ] Run a command-producing task and capture command execution JSONL.
- [ ] Run a file-editing task and capture file change JSONL.
- [ ] Run a failing command task and capture failure JSONL.
- [ ] Run a sandbox-denial or approval-required case and capture JSONL.
- [ ] Run image input with `--image` and capture JSONL.
- [ ] Verify `codex exec resume <id> --json <prompt>` with the id emitted by the first run.
- [ ] Verify that a resumed run sees prior conversation context without agent-manager replaying old events.
- [ ] Verify that a resumed run still works after container restart when `/app/.codex` is persisted.
- [ ] Decide whether `thread_id`, `session_id`, or another value is the correct persisted resume id.
- [ ] Verify whether `--output-last-message` is useful as a final-message fallback.
- [ ] Decide whether MVP needs `codex exec` only, app-server, or SDK. Default decision: `codex exec --json` unless research disproves it.

### Phase 1: Provider-neutral metadata

- [x] Add `provider` and `kind` to `Instance`, `InstanceRecord`, summaries, and create/update bodies.
- [x] Preserve backward compatibility for `instance_type`.
- [x] Migrate old `instance_type == "claude"` records to `kind = "agent"`, `provider = "claude"`.
- [x] Migrate old `instance_type == "loop"` records to `kind = "loop"` with provider defaults preserved for backing sessions.
- [x] Add tests for persistence migration from old records.
- [x] Add provider field to frontend instance summaries without changing behavior.
- [x] Update Go `InstanceInfo` to tolerate and optionally expose `provider` and `kind`.

### Phase 2: Move Claude into an adapter

- [x] Create `providers/base.py` with `AgentRuntime`, `AgentInput`, `AgentConfig`, and normalized event types.
- [x] Create `providers/registry.py`.
- [x] Move Claude SDK setup into `providers/claude.py`.
- [x] Move Claude multimodal payload creation into `providers/claude.py`.
- [x] Move Claude event translation into `providers/claude.py` as the Claude adapter's translator.
- [x] Keep Docker MCP option injection isolated in the Claude provider.
- [x] Keep the existing UI/API behavior unchanged.
- [x] Add focused unit tests for Claude event translation using SDK message fixtures or lightweight fakes.
- [x] Add a fake runtime test proving generic `Instance` handles status/history/subscribers independent of provider.

### Phase 3: Provider capabilities and auth

- [x] Add `/api/providers`.
- [x] Add `/api/providers/{provider}/models`.
- [x] Add `/api/providers/{provider}/auth/status`.
- [x] Add `/api/providers/{provider}/auth/login`.
- [x] Refactor Claude auth behind provider auth.
- [x] Add Codex auth status shape and disabled login placeholder; enable real login when the Codex runtime is installed.
- [x] Keep `/api/auth/*` as Claude-compatible wrappers during transition.
- [x] Update the new-instance dialog to select provider first.
- [x] Update the permissions/settings panel to render controls from provider capabilities.
- [x] Keep `/api/models` temporarily as `provider=claude` compatibility.

### Phase 4: Codex runtime adapter

- [x] Add Codex CLI installation to Dockerfile.
- [x] Add a `codex-auth` or shared config volume mounted at `/app/.codex`.
- [x] Ensure `/app/.codex` is writable by the app user in fresh named volumes.
- [x] Implement `CodexRuntime` command construction from provider runtime options.
- [x] Use fresh `codex exec` for instances without `session_id`.
- [x] Use `codex exec resume <session_id>` for instances with `session_id`.
- [x] Implement concurrent stdout JSONL parsing and stderr capture.
- [x] Implement Codex event translator from fixture files.
- [x] Persist Codex resume id from JSONL.
- [x] Implement resumed turns with `codex exec resume`.
- [x] Implement abort by terminating the subprocess and marking the current turn interrupted.
- [x] Implement temp-file image handling.
- [x] Add error handling for malformed JSONL, missing binary, unauthenticated CLI, and nonzero process exit.
- [x] Preserve raw provider details under `data.provider_raw` only in debug or explicitly selected events.
- [ ] Add integration smoke test for a simple Codex prompt when auth is available.

### Phase 5: Provider-aware file tabs and teams

- [ ] Make rules endpoint provider-aware.
- [ ] Make plans endpoint provider-aware or move plans to an agent-manager-owned path.
- [ ] Make memory endpoint provider-aware or hide unsupported memory tabs based on capabilities.
- [ ] Update team YAML to accept provider/runtime options per agent.
- [ ] Add provider to orchestrator team descriptions as optional context.
- [ ] Add mixed-provider team tests.

### Phase 6: Cleanup

- [ ] Rename `permission_mode` endpoints/fields to provider-neutral runtime options, leaving aliases only where needed.
- [ ] Rename `instance_type` to `kind` throughout frontend and backend.
- [ ] Remove Claude-specific labels from generic UI.
- [ ] Update README and Docker docs to describe both providers.
- [ ] Remove compatibility wrappers only after migration is complete and documented.

## Testing Strategy

Backend:

- persistence migration tests for old/new records;
- provider registry tests;
- generic `Instance` lifecycle tests with a fake runtime;
- Claude translation tests;
- Codex JSONL translation tests using recorded fixtures;
- auth status tests per provider;
- API compatibility tests for old `/api/models` and `/api/auth/status` while those remain.

Frontend:

- provider selector behavior in new-instance dialog;
- capabilities-driven permission controls;
- model dropdown per provider;
- auth banner/login dialog per provider;
- rendering of normalized Codex events in terminal pane.

Integration:

- create a Claude instance and send a prompt;
- create a Codex instance and send a prompt;
- abort a running Codex turn;
- restart/reload runtime options;
- restart container and resume both provider records;
- run a team with mixed Claude/Codex children and verify the Go orchestrator can send/read/status-check both.

## Main Risks

- Codex session resume semantics may differ from Claude's long-lived SDK client. Keep resume handling adapter-local and write fixtures before committing to a session-id extraction method.
- Codex provider-owned session storage must be persisted separately from agent-manager's UI event log. If `/app/.codex` is not mounted or is not writable, agent-manager may keep a resume id that Codex can no longer resolve.
- Codex JSONL event names may change. Keep a small compatibility parser and store raw events only for diagnostics.
- Claude and Codex permission models are not one-to-one. Do not force them into the same enum; expose provider capabilities and map only high-level concepts in the UI.
- Existing `instance_type` overload will cause confusion if not handled early. Add `provider` and `kind` before implementing Codex runtime.
- File tabs are currently Claude-specific. Provider capabilities should decide which tabs are shown rather than making Codex pretend to have Claude paths.

## Source Notes

- Local repo inspection on 2026-05-22.
- Local Codex CLI help from `codex-cli 0.133.0`.
- Official Codex CLI docs: https://developers.openai.com/codex/cli
- Official Codex non-interactive/JSONL docs: https://developers.openai.com/codex/noninteractive
- Official Codex SDK docs: https://developers.openai.com/codex/sdk
- Official Codex app-server docs: https://developers.openai.com/codex/app-server
- Official Codex MCP/Agents SDK integration docs: https://developers.openai.com/codex/guides/agents-sdk
- Official Codex CLI command reference: https://developers.openai.com/codex/cli/reference
