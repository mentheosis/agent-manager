# UI File Artifacts Plan

## Goal

Allow an agent to intentionally present an existing file from disk to the user as a first-class UI event, instead of relying on plain text paths in assistant messages or tool output.

The first target is screenshots and generated images, but the design should support other safe file previews later.

## Current State

Agent Manager already has normalized stream events such as:

- `assistant_text`
- `thinking`
- `tool_use`
- `tool_result`
- `result`
- `error`
- `artifact` for detected image files

The current image artifact support detects some image paths from Codex events and scans Codex generated-image directories. That is useful as a fallback, but it is not yet a deliberate model-facing protocol for posting arbitrary approved disk files to the UI.

## Proposed Event Shape

Use a provider-neutral event:

```json
{
  "type": "artifact",
  "artifact_type": "image",
  "artifact_id": "opaque-id",
  "path": "/app/project/reports/screenshot.png",
  "title": "Mobile homepage screenshot",
  "mime_type": "image/png",
  "source": "codex"
}
```

For non-image files later:

```json
{
  "type": "artifact",
  "artifact_type": "file",
  "artifact_id": "opaque-id",
  "path": "/app/project/reports/output.json",
  "title": "Validation output",
  "mime_type": "application/json",
  "source": "claude"
}
```

## Model-Facing Protocol

Add a short instruction to both Claude and Codex provider prompts:

```text
When you want to show the user a local file that already exists on disk, emit:
[[agent-manager:artifact path="/absolute/path/to/file.png" title="Short title"]]

Use this for screenshots, generated images, reports, or other files the user should inspect in the UI.
```

Agent Manager should parse this directive from assistant text and tool output, remove or de-emphasize the directive in rendered text, and emit a separate `artifact` event.

Initial supported attributes:

- `path`: required absolute path.
- `title`: optional display title.
- `type`: optional hint, default inferred from extension/MIME.

## Backend Serving

The browser must not read raw filesystem paths directly. The backend should serve artifact files through an allowlisted route:

```text
GET /api/artifacts/{artifact_id}
```

The server resolves `artifact_id` to a local path and validates:

- path exists
- path is a regular file
- path is under an allowed root
- extension/MIME is supported
- file size is below a configured preview limit

Allowed roots should start conservative:

- the instance workspace
- configured `add_dirs`
- `/app/.codex/generated_images`
- `~/.codex/generated_images`
- `/tmp`
- `/var/lib/agent-manager`

The event should keep `path` for transparency, but the UI should load from the artifact route.

## UI Rendering

Render artifacts as distinct event rows.

For images:

- inline preview
- animated GIF playback through the browser's native image rendering
- title and path metadata
- click to open full image in a new tab

For other files later:

- filename/title
- MIME/type label
- size if available
- open/download action
- optional inline preview for text, JSON, markdown, SVG, and HTML-as-source

Do not render arbitrary HTML as live page content inside the conversation view.

## Provider Integration

The parser should be provider-neutral and run after each normalized event is produced.

Recommended pipeline:

1. Provider emits `assistant_text` or `tool_result`.
2. Artifact directive parser extracts `[[agent-manager:artifact ...]]`.
3. Parser emits one or more `artifact` events.
4. Original text is either cleaned of directives or left unchanged behind a feature flag.
5. Registry persists both the cleaned text event and artifact event.
6. WebSocket sends both events to the UI.

This keeps Claude and Codex behavior consistent and avoids relying on provider-specific image or file message formats.

## Security Rules

Do not allow the model to expose arbitrary host files by path alone.

Required controls:

- allowlist roots per instance
- only regular files
- no symlink escape outside allowed roots
- deny hidden credentials and common secret filenames
- cap preview file size
- only render supported MIME types inline
- use attachment/download behavior for unknown file types

The first implementation should support images only. Expand file types after the path validation and event flow are stable.

## Implementation Steps

1. Add a provider-neutral artifact parser module.
2. Define the directive grammar and tests.
3. Apply the parser in the instance publish path so it works for Claude and Codex.
4. Extend artifact serving to validate against the active instance workspace and add_dirs, not only global roots.
5. Render image artifacts in the terminal pane.
6. Add provider instructions telling models how to emit artifact directives.
7. Add tests for directive parsing, path rejection, artifact serving, and UI event shape.

## Open Questions

- Should directives be stripped from the visible assistant/tool text by default?
- Should artifact events be nested under the tool/assistant event that produced them, or appear as sibling events?
- Should non-image files be downloadable only at first, or should text/JSON/markdown get inline previews immediately?
- Should the UI support multiple artifacts in one compact gallery event?
