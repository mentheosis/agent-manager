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

`src/agent_manager/auth.py` is fully Claude-specific: it checks `~/.claude/.credentials.json` and runs `claude login` inside a pty.

`src/agent_manager/files.py` is mostly generic except memory support, which encodes paths using Claude's `~/.claude/projects/.../memory` layout.

### Frontend

The static UI expects a single provider:

- `am-new-dialog.js` creates "Single Claude instance", hard-codes Claude permission modes, Claude model examples, and merges YAML permissions into `.claude/settings.json`.
- `am-permissions-panel.js` exposes model, Claude permission mode, and allowed directories.
- `am-auth-banner.js` and `am-login-dialog.js` assume one global Claude auth status and run `claude login`.
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

Local `codex-cli 0.133.0` is installed in this environment. Its help shows these useful options:

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

Official OpenAI docs identify `gpt-5.2-codex` as the coding-optimized model for Codex-like agentic coding environments and note support for image input, streaming, function calling, structured outputs, and Responses API use. Current Codex CLI docs also document `codex exec --json` as the programmatic JSONL mode.

Because the Codex CLI/app-server surface is evolving, the implementation should hide Codex details behind a narrow adapter. Start with `codex exec --json` if no stable Python SDK is available, but design so that a future Codex SDK/app-server implementation can replace the subprocess adapter without changing the registry, API, UI event stream, or orchestrator.

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

Start with a Codex subprocess adapter unless a stable Python SDK is chosen before implementation.

For each turn, spawn something close to:

```text
codex exec --json -C <cwd> [--model <model>] [--add-dir <dir>...] [--sandbox <mode>] [--ask-for-approval <policy>] <prompt>
```

For resumed turns, use the documented/current resume command once verified against JSONL output:

```text
codex exec resume <session_id> --json <prompt>
```

The adapter should:

- parse stdout as JSONL into provider-specific raw events;
- translate those into the existing normalized UI events;
- publish a `system_init` equivalent with provider/model/runtime metadata at turn start;
- capture Codex session id if JSONL exposes it; if not, inspect session files under `$CODEX_HOME` in an adapter-only helper and treat this as a known implementation risk;
- stream stderr as diagnostic `error` or `provider_log` events only when useful;
- map process exit to `result` with `is_error`, duration, and available usage metadata;
- support abort by terminating the child process and publishing `aborted`;
- handle images by writing temporary files for base64 uploads and passing `--image` arguments, then deleting temp files after the process exits.

Codex event translation should target the existing event contract, not the UI directly. Expected mapping to verify with fixtures:

- agent-message deltas -> `assistant_text`;
- command or tool start -> `tool_use`;
- command output deltas / tool result events -> `tool_result` or a provider-specific normalized `tool_result_delta` if incremental output is too noisy;
- reasoning summaries, if present -> `thinking`;
- turn completion -> `result`;
- errors -> `error` and final `result.is_error = true`.

If Codex JSONL does not produce stable tool ids, synthesize per-turn ids such as `codex-{turn_seq}-{item_id}` so nested tool results keep working in `am-terminal-pane.js`.

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
- Codex: either static recommended model list (`gpt-5.2-codex`, prior Codex variants) or OpenAI model API if `OPENAI_API_KEY` is available. Avoid mixing Anthropic and OpenAI IDs in one dropdown without a provider selector.

### 6. Auth refactor

Generalize `AuthRegistry` into provider auth sessions:

- Claude provider:
  - credentials path: `~/.claude/.credentials.json`;
  - login command: `claude login`;
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
  "model": "gpt-5.2-codex",
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
    model: gpt-5.2-codex
    path: /repo
```

Default provider should remain Claude for backward compatibility.

## Implementation Phases

### Phase 1: Provider-neutral metadata

- Add `provider` and `kind` to `Instance`, `InstanceRecord`, summaries, and create/update bodies.
- Preserve backward compatibility for `instance_type`.
- Add tests for persistence migration from old records.
- Add provider field to frontend instance summaries without changing behavior.

### Phase 2: Move Claude into an adapter

- Create provider interfaces and registry.
- Move Claude SDK setup, multimodal payload creation, event translation, and Docker MCP option injection into `ClaudeRuntime`.
- Keep the existing UI/API behavior unchanged.
- Add focused unit tests for Claude event translation using SDK message fixtures or lightweight fakes.

### Phase 3: Provider capabilities and auth

- Add `/api/providers` and provider-scoped model/auth endpoints.
- Refactor Claude auth behind provider auth.
- Update the new-instance dialog and permissions panel to render controls from capabilities.
- Keep `/api/auth/*` as Claude-compatible wrappers during transition.

### Phase 4: Codex runtime adapter

- Add Codex CLI installation to Dockerfile.
- Add a `codex-auth` or shared config volume mounted at `/app/.codex`.
- Implement `CodexRuntime` with `codex exec --json`.
- Verify and fixture actual JSONL output for:
  - normal assistant text,
  - shell command execution,
  - file edit,
  - approval/sandbox denial,
  - turn failure,
  - image input if supported.
- Map Codex events to normalized events and preserve raw provider details under `data.provider_raw` only when needed for debugging.

### Phase 5: Provider-aware file tabs and teams

- Make rules/plans/memory endpoints provider-aware.
- Update team YAML to accept provider/runtime options per agent.
- Include provider in Go `InstanceInfo`.
- Add mixed-provider team tests.

### Phase 6: Cleanup

- Rename `permission_mode` endpoints/fields to provider-neutral runtime options, leaving aliases only where needed.
- Rename `instance_type` to `kind` throughout frontend and backend.
- Remove Claude-specific labels from generic UI.
- Update README and Docker docs to describe both providers.

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
- Codex JSONL event names may change. Keep a small compatibility parser and store raw events only for diagnostics.
- Claude and Codex permission models are not one-to-one. Do not force them into the same enum; expose provider capabilities and map only high-level concepts in the UI.
- Existing `instance_type` overload will cause confusion if not handled early. Add `provider` and `kind` before implementing Codex runtime.
- File tabs are currently Claude-specific. Provider capabilities should decide which tabs are shown rather than making Codex pretend to have Claude paths.

## Source Notes

- Local repo inspection on 2026-05-22.
- Local Codex CLI help from `codex-cli 0.133.0`.
- OpenAI model docs for `gpt-5.2-codex`: https://developers.openai.com/api/docs/models/gpt-5.2-codex
- OpenAI latest-model guide recommending `gpt-5.2-codex` for Codex-like agentic coding environments: https://platform.openai.com/docs/guides/latest-model
- Codex CLI documentation for `codex exec --json` and non-interactive execution: https://www.mintlify.com/openai/codex/cli/exec
