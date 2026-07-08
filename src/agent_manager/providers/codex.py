from __future__ import annotations

import asyncio
import base64
import binascii
import datetime as dt
import json
import logging
import shutil
import tempfile
from collections.abc import AsyncIterator
from pathlib import Path
from typing import Any

from agent_manager.artifacts import artifact_instruction, image_artifact_event, is_image_path

from .base import AgentConfig, AgentEvent, AgentInput, build_prompt_with_context, format_memory_block
from .codex_events import translate_codex_event, translate_codex_transcript_event
from .codex_metadata import fetch_codex_runtime_metadata

log = logging.getLogger(__name__)

CODEX_STREAM_LIMIT = 10 * 1024 * 1024
_STREAM_LIMIT_ERROR = "Separator is found, but chunk is longer than limit"


class CodexRuntime:
    provider = "codex"

    def __init__(self, config: AgentConfig) -> None:
        self.config = config
        self._session_id = config.session_id
        self._proc: asyncio.subprocess.Process | None = None

    async def start(self) -> None:
        if shutil.which("codex") is None:
            raise RuntimeError("codex CLI is not installed")

    async def run_turn(self, message: AgentInput) -> AsyncIterator[AgentEvent]:
        stderr_chunks: list[str] = []
        with tempfile.TemporaryDirectory(prefix="agent-manager-codex-") as tmpdir:
            existing_generated_images = self._generated_image_paths()
            reported_generated_images: set[Path] = set()
            try:
                image_paths = self._write_images(message.images, Path(tmpdir))
            except ValueError as e:
                yield {"type": "error", "message": str(e)}
                yield {"type": "result", "subtype": "error", "is_error": True}
                return

            cmd = self._build_command(self._prompt_with_context(message.text), image_paths)
            system_context = await self._system_init_context()
            diagnostics: dict[str, Any] = self._latest_transcript_diagnostics() or {}
            log.info("instance %s: starting Codex command: %s", self.config.title, cmd[:4])

            try:
                self._proc = await asyncio.create_subprocess_exec(
                    *cmd,
                    cwd=self.config.cwd,
                    stdout=asyncio.subprocess.PIPE,
                    stderr=asyncio.subprocess.PIPE,
                    limit=CODEX_STREAM_LIMIT,
                )
            except FileNotFoundError:
                yield {"type": "error", "message": "codex CLI is not installed"}
                yield {"type": "result", "subtype": "error", "is_error": True}
                return
            except OSError as e:
                yield {"type": "error", "message": f"failed to start codex: {e}"}
                yield {"type": "result", "subtype": "error", "is_error": True}
                return

            stderr_task = asyncio.create_task(self._read_stderr(stderr_chunks))
            transcript_queue: asyncio.Queue[AgentEvent] = asyncio.Queue()
            transcript_task = self._start_transcript_tail(transcript_queue, system_context, diagnostics)
            seen_assistant_texts: set[str] = set()
            seen_result = {"emitted": False}
            saw_result = False
            try:
                assert self._proc.stdout is not None
                stdout_task: asyncio.Task[bytes] | None = asyncio.create_task(self._proc.stdout.readline())
                while stdout_task is not None:
                    wait_tasks: set[asyncio.Task[Any]] = {stdout_task}
                    transcript_get: asyncio.Task[AgentEvent] | None = None
                    if transcript_task is not None:
                        transcript_get = asyncio.create_task(transcript_queue.get())
                        wait_tasks.add(transcript_get)
                    done, _ = await asyncio.wait(wait_tasks, return_when=asyncio.FIRST_COMPLETED)

                    if transcript_get is not None and transcript_get in done:
                        event = transcript_get.result()
                        self._record_event_diagnostics(event, diagnostics)
                        if event.get("type") == "result":
                            saw_result = True
                        if _should_emit_event(event, seen_assistant_texts, seen_result):
                            yield event
                        if _is_terminal_result(event):
                            await self._terminate()
                            return
                        continue
                    if transcript_get is not None:
                        transcript_get.cancel()
                        try:
                            await transcript_get
                        except asyncio.CancelledError:
                            pass

                    if stdout_task not in done:
                        continue
                    line = stdout_task.result()
                    if not line:
                        stdout_task = None
                        break
                    text = line.decode("utf-8", errors="replace").strip()
                    stdout_task = asyncio.create_task(self._proc.stdout.readline())
                    if not text:
                        continue
                    try:
                        raw = json.loads(text)
                    except json.JSONDecodeError as e:
                        if not text.startswith("{"):
                            stderr_chunks.append(text + "\n")
                            continue
                        yield {"type": "error", "message": f"malformed codex JSONL: {e}: {text[:200]}"}
                        continue
                    if not isinstance(raw, dict):
                        yield {"type": "error", "message": f"unexpected codex JSONL payload: {text[:200]}"}
                        continue
                    for event in translate_codex_event(raw, system_context=system_context):
                        self._capture_session_id(event)
                        if transcript_task is None:
                            transcript_task = self._start_transcript_tail(
                                transcript_queue,
                                system_context,
                                diagnostics,
                            )
                        self._record_event_diagnostics(event, diagnostics)
                        if event.get("type") == "result":
                            event = _merge_result_diagnostics(event, diagnostics)
                        if event.get("type") == "artifact":
                            self._track_artifact_path(event, reported_generated_images)
                        if event.get("type") == "result":
                            saw_result = True
                            for image_event in self._new_generated_image_events(
                                existing_generated_images,
                                reported_generated_images,
                            ):
                                yield image_event
                        if _should_emit_event(event, seen_assistant_texts, seen_result):
                            yield event
                        if _is_terminal_result(event):
                            await self._terminate()
                            return

                while not transcript_queue.empty():
                    event = transcript_queue.get_nowait()
                    self._record_event_diagnostics(event, diagnostics)
                    if event.get("type") == "result":
                        event = _merge_result_diagnostics(event, diagnostics)
                    if event.get("type") == "artifact":
                        self._track_artifact_path(event, reported_generated_images)
                    if event.get("type") == "result":
                        saw_result = True
                    if _should_emit_event(event, seen_assistant_texts, seen_result):
                        yield event
                    if _is_terminal_result(event):
                        await self._terminate()
                        return

                returncode = await self._proc.wait()
                if transcript_task is not None and not transcript_task.done():
                    try:
                        await asyncio.wait_for(transcript_task, timeout=1)
                    except asyncio.TimeoutError:
                        pass
                while not transcript_queue.empty():
                    event = transcript_queue.get_nowait()
                    self._record_event_diagnostics(event, diagnostics)
                    if event.get("type") == "result":
                        event = _merge_result_diagnostics(event, diagnostics)
                    if event.get("type") == "artifact":
                        self._track_artifact_path(event, reported_generated_images)
                    if event.get("type") == "result":
                        saw_result = True
                    if _should_emit_event(event, seen_assistant_texts, seen_result):
                        yield event
                    if _is_terminal_result(event):
                        await self._terminate()
                        return
                await stderr_task
                if returncode != 0:
                    stderr = "".join(stderr_chunks).strip()
                    message_text = stderr or f"codex exited with code {returncode}"
                    yield {"type": "error", "message": message_text}
                    for image_event in self._new_generated_image_events(
                        existing_generated_images,
                        reported_generated_images,
                    ):
                        yield image_event
                    yield {
                        "type": "result",
                        "subtype": "error",
                        "is_error": True,
                        "session_id": self._session_id,
                        **_result_diagnostics(diagnostics, returncode=returncode, stderr=stderr),
                    }
                elif not saw_result:
                    for image_event in self._new_generated_image_events(
                        existing_generated_images,
                        reported_generated_images,
                    ):
                        yield image_event
                    yield {
                        "type": "result",
                        "subtype": "success",
                        "is_error": False,
                        "session_id": self._session_id,
                        **_result_diagnostics(diagnostics),
                    }
            except asyncio.CancelledError:
                await self._terminate()
                raise
            except (asyncio.LimitOverrunError, ValueError) as e:
                if not _is_stream_limit_error(e):
                    raise
                await self._terminate()
                yield {"type": "error", "message": _stream_limit_message("stdout", e)}
                yield {
                    "type": "result",
                    "subtype": "error",
                    "is_error": True,
                    "session_id": self._session_id,
                    **_result_diagnostics(diagnostics, error_message=_stream_limit_message("stdout", e)),
                }
            finally:
                if transcript_task is not None:
                    transcript_task.cancel()
                    try:
                        await transcript_task
                    except asyncio.CancelledError:
                        pass
                if not stderr_task.done():
                    stderr_task.cancel()
                    try:
                        await stderr_task
                    except asyncio.CancelledError:
                        pass
                self._proc = None

    async def close(self) -> None:
        await self._terminate()

    def _build_command(self, prompt: str, image_paths: list[Path]) -> list[str]:
        # Build the developer_instructions config override once; same value for
        # fresh + resume so the model sees a stable system block (better caching).
        dev_instructions = self._developer_instructions()

        if self._session_id:
            cmd = ["codex", "exec", "resume", "--json", "--skip-git-repo-check"]
            if dev_instructions:
                cmd.extend(["-c", dev_instructions])
            if self._bypass_sandbox():
                cmd.append("--dangerously-bypass-approvals-and-sandbox")
            if self.config.model:
                cmd.extend(["--model", self.config.model])
            for image in image_paths:
                cmd.extend(["--image", str(image)])
            if image_paths:
                cmd.append("--")
            cmd.extend([self._session_id, prompt])
            return cmd

        cmd = [
            "codex",
            "exec",
            "--json",
            "--cd",
            self.config.cwd,
        ]
        if dev_instructions:
            cmd.extend(["-c", dev_instructions])
        if self._bypass_sandbox():
            cmd.append("--dangerously-bypass-approvals-and-sandbox")
        else:
            cmd.extend(["--sandbox", self._sandbox_mode()])
        cmd.append("--skip-git-repo-check")
        if self.config.model:
            cmd.extend(["--model", self.config.model])
        for add_dir in self.config.add_dirs:
            cmd.extend(["--add-dir", add_dir])
        for image in image_paths:
            cmd.extend(["--image", str(image)])
        if image_paths:
            cmd.append("--")
        cmd.append(prompt)
        return cmd

    def _developer_instructions(self) -> str | None:
        """Build the TOML config override for codex's developer_instructions.

        Returns a string like:
            developer_instructions="<memory>...</memory>\\n\\n[artifact instr]"

        Suitable for passing via `codex -c <value>`. The value is parsed as TOML
        by codex, so we JSON-encode the inner string (JSON basic-string escapes
        are a subset of TOML basic-string escapes).

        Returns None if nothing to inject.
        """
        parts: list[str] = []
        memory_block = format_memory_block(self.config.memory_file)
        if memory_block:
            parts.append(memory_block)
        # Artifact instruction is stable per build — include it always.
        parts.append(artifact_instruction().strip())

        if not parts:
            return None

        combined = "\n\n".join(parts)
        # json.dumps gives us a properly-escaped basic string; TOML accepts it.
        escaped = json.dumps(combined)
        return f"developer_instructions={escaped}"

    def _prompt_with_context(self, text: str) -> str:
        """Build the per-turn prompt.

        For Codex, both memory_file content and the artifact instruction live in
        the `developer_instructions` system block (passed via -c flag), so the
        per-turn prompt is just the user's actual input.
        """
        return build_prompt_with_context(
            text,
            memory_file=None,
            include_artifact_instruction=False,
        )

    def _sandbox_mode(self) -> str:
        mode = (self.config.permission_mode or "").strip()
        if mode in {"read-only", "workspace-write", "danger-full-access"}:
            return mode
        if mode == "plan":
            return "read-only"
        if mode == "bypassPermissions":
            return "danger-full-access"
        return "workspace-write"

    def _bypass_sandbox(self) -> bool:
        mode = (self.config.permission_mode or "").strip()
        return mode in {"danger-full-access", "bypassPermissions"}

    async def _system_init_context(self) -> dict[str, Any]:
        context: dict[str, Any] = {
            "cwd": self.config.cwd,
            "permission_mode": self.config.permission_mode,
            "sandbox": self._sandbox_mode(),
            "resume": bool(self._session_id),
            "command": "codex exec resume" if self._session_id else "codex exec",
        }
        if self.config.add_dirs:
            context["add_dirs"] = list(self.config.add_dirs)
        if self.config.model:
            context["requested_model"] = self.config.model
            context["active_model_label"] = self.config.model
        else:
            context.update(await fetch_codex_runtime_metadata(cwd=self.config.cwd))
        rate_limits = self._latest_rate_limits()
        if rate_limits:
            context["rate_limits"] = rate_limits
        return {key: value for key, value in context.items() if value not in (None, "", [])}

    def _capture_session_id(self, event: AgentEvent) -> None:
        sid = event.get("session_id")
        if isinstance(sid, str) and sid:
            self._session_id = sid
            return
        data = event.get("data")
        if isinstance(data, dict):
            sid = data.get("session_id")
            if isinstance(sid, str) and sid:
                self._session_id = sid

    @staticmethod
    def _record_event_diagnostics(event: AgentEvent, diagnostics: dict[str, Any]) -> None:
        if event.get("type") == "result":
            usage = event.get("usage")
            if isinstance(usage, dict):
                diagnostics["usage"] = usage
            context = event.get("context")
            if isinstance(context, dict):
                diagnostics["context"] = context
        elif event.get("type") == "error":
            message = event.get("message")
            if isinstance(message, str) and message:
                diagnostics["error_message"] = message
            data = event.get("data")
            if isinstance(data, dict):
                diagnostics["error_data"] = data

    def _write_images(self, images: list[dict[str, Any]], tmpdir: Path) -> list[Path]:
        paths: list[Path] = []
        for idx, image in enumerate(images):
            data = image.get("data")
            if not isinstance(data, str):
                raise ValueError("image data must be base64-encoded text")
            media_type = str(image.get("media_type") or "image/png")
            suffix = _suffix_for_media_type(media_type)
            path = tmpdir / f"image-{idx}{suffix}"
            try:
                path.write_bytes(base64.b64decode(data, validate=True))
            except (binascii.Error, OSError) as e:
                raise ValueError(f"failed to write image input: {e}") from e
            paths.append(path)
        return paths

    def _generated_image_paths(self) -> set[Path]:
        roots = [
            Path("/app/.codex/generated_images"),
            Path.home() / ".codex" / "generated_images",
        ]
        paths: set[Path] = set()
        for root in roots:
            if not root.is_dir():
                continue
            try:
                for path in root.glob("**/*"):
                    if path.is_file() and is_image_path(path):
                        paths.add(path.resolve())
            except OSError:
                continue
        return paths

    def _new_generated_image_events(
        self,
        existing: set[Path],
        reported: set[Path],
    ) -> list[AgentEvent]:
        current = self._generated_image_paths()
        new_paths = sorted(current - existing - reported)
        events: list[AgentEvent] = []
        for path in new_paths:
            reported.add(path)
            events.append(image_artifact_event(path, source="codex"))
        return events

    @staticmethod
    def _track_artifact_path(event: AgentEvent, reported: set[Path]) -> None:
        path = event.get("path")
        if isinstance(path, str) and path:
            try:
                reported.add(Path(path).resolve())
            except OSError:
                pass

    async def _read_stderr(self, chunks: list[str]) -> None:
        if self._proc is None or self._proc.stderr is None:
            return
        try:
            async for data in self._proc.stderr:
                chunks.append(data.decode("utf-8", errors="replace"))
        except (asyncio.LimitOverrunError, ValueError) as e:
            if not _is_stream_limit_error(e):
                raise
            chunks.append(_stream_limit_message("stderr", e))

    def _start_transcript_tail(
        self,
        queue: asyncio.Queue[AgentEvent],
        system_context: dict[str, Any],
        diagnostics: dict[str, Any],
    ) -> asyncio.Task[None] | None:
        path = self._codex_session_path()
        if path is None:
            return None
        try:
            offset = path.stat().st_size
        except OSError:
            return None
        return asyncio.create_task(
            self._tail_codex_session_transcript(path, offset, queue, system_context, diagnostics),
            name=f"codex-transcript-tail:{self.config.title}",
        )

    def _codex_session_path(self) -> Path | None:
        if not self._session_id:
            return None
        sessions_dir = Path.home() / ".codex" / "sessions"
        try:
            matches = sorted(sessions_dir.glob(f"**/*{self._session_id}*.jsonl"))
        except OSError:
            return None
        return matches[-1] if matches else None

    def _latest_rate_limits(self) -> dict[str, Any] | None:
        diagnostics = self._latest_transcript_diagnostics()
        rate_limits = diagnostics.get("rate_limits") if diagnostics else None
        return rate_limits if isinstance(rate_limits, dict) else None

    def _latest_transcript_diagnostics(self) -> dict[str, Any] | None:
        path = self._codex_session_path()
        if path is None:
            return None
        try:
            with path.open("rb") as f:
                f.seek(0, 2)
                size = f.tell()
                f.seek(max(size - 4 * 1024 * 1024, 0))
                chunk = f.read().decode("utf-8", errors="replace")
        except OSError:
            return None

        diagnostics: dict[str, Any] = {}
        for line in reversed(chunk.splitlines()):
            try:
                raw = json.loads(line)
            except json.JSONDecodeError:
                continue
            _update_diagnostics_from_transcript_raw(raw, diagnostics)
            if ("usage" in diagnostics and "context" in diagnostics) or "rate_limits" in diagnostics:
                return diagnostics
        return diagnostics or None

    async def _tail_codex_session_transcript(
        self,
        path: Path,
        offset: int,
        queue: asyncio.Queue[AgentEvent],
        system_context: dict[str, Any],
        diagnostics: dict[str, Any],
    ) -> None:
        while True:
            try:
                size = path.stat().st_size
                if size > offset:
                    with path.open("r", encoding="utf-8", errors="replace") as f:
                        f.seek(offset)
                        chunk = f.read()
                        offset = f.tell()
                    for line in chunk.splitlines():
                        if not line.strip():
                            continue
                        try:
                            raw = json.loads(line)
                        except json.JSONDecodeError:
                            continue
                        _update_diagnostics_from_transcript_raw(raw, diagnostics)
                        for event in translate_codex_transcript_event(raw, system_context=system_context):
                            queue.put_nowait(event)
                elif size < offset:
                    offset = size
            except OSError:
                pass
            if self._proc is None or self._proc.returncode is not None:
                break
            await asyncio.sleep(0.25)

    async def _terminate(self) -> None:
        proc = self._proc
        if proc is None or proc.returncode is not None:
            return
        try:
            proc.terminate()
            await asyncio.wait_for(proc.wait(), timeout=2)
        except (ProcessLookupError, asyncio.TimeoutError):
            try:
                proc.kill()
            except ProcessLookupError:
                return
            await proc.wait()


def _suffix_for_media_type(media_type: str) -> str:
    if media_type == "image/jpeg":
        return ".jpg"
    if media_type == "image/webp":
        return ".webp"
    if media_type == "image/gif":
        return ".gif"
    return ".png"


def _is_stream_limit_error(e: BaseException) -> bool:
    return isinstance(e, asyncio.LimitOverrunError) or _STREAM_LIMIT_ERROR in str(e)


def _update_diagnostics_from_transcript_raw(raw: dict[str, Any], diagnostics: dict[str, Any]) -> None:
    payload = raw.get("payload")
    if not isinstance(payload, dict):
        return

    payload_type = payload.get("type")
    if payload_type == "task_started":
        context_window = payload.get("model_context_window")
        if isinstance(context_window, int):
            context = dict(diagnostics.get("context") or {})
            context["context_window"] = context_window
            diagnostics["context"] = _with_context_percent(context)
        return

    if payload_type != "token_count":
        return

    timestamp = raw.get("timestamp")
    rate_limits = payload.get("rate_limits")
    if isinstance(rate_limits, dict):
        normalized = _normalize_rate_limits(rate_limits)
        if isinstance(timestamp, str):
            normalized["observed_at"] = timestamp
        diagnostics["rate_limits"] = normalized

    info = payload.get("info")
    if not isinstance(info, dict):
        return

    total_usage = info.get("total_token_usage")
    if isinstance(total_usage, dict):
        diagnostics["usage"] = dict(total_usage)
    last_usage = info.get("last_token_usage")

    context: dict[str, Any] = dict(diagnostics.get("context") or {})
    context_window = info.get("model_context_window") or payload.get("model_context_window")
    if isinstance(context_window, int):
        context["context_window"] = context_window
    context_usage = last_usage if isinstance(last_usage, dict) else total_usage
    if isinstance(context_usage, dict):
        total_tokens = context_usage.get("total_tokens")
        if isinstance(total_tokens, int):
            context["total_tokens"] = total_tokens
        elif isinstance(context_usage.get("input_tokens"), int) or isinstance(context_usage.get("output_tokens"), int):
            context["total_tokens"] = int(context_usage.get("input_tokens") or 0) + int(context_usage.get("output_tokens") or 0)
        context["source"] = "last_token_usage" if isinstance(last_usage, dict) else "total_token_usage"

    if isinstance(timestamp, str):
        context["observed_at"] = timestamp

    if context:
        diagnostics["context"] = _with_context_percent(context)


def _with_context_percent(context: dict[str, Any]) -> dict[str, Any]:
    total_tokens = context.get("total_tokens")
    context_window = context.get("context_window")
    if isinstance(total_tokens, int) and isinstance(context_window, int) and context_window > 0:
        context["used_percent"] = round((total_tokens / context_window) * 100, 1)
        context["remaining_tokens"] = max(context_window - total_tokens, 0)
    return context


def _result_diagnostics(
    diagnostics: dict[str, Any],
    *,
    returncode: int | None = None,
    stderr: str | None = None,
    error_message: str | None = None,
) -> dict[str, Any]:
    result: dict[str, Any] = {}
    usage = diagnostics.get("usage")
    if isinstance(usage, dict):
        result["usage"] = usage
    context = diagnostics.get("context")
    if isinstance(context, dict):
        result["context"] = context
    rate_limits = diagnostics.get("rate_limits")
    if isinstance(rate_limits, dict):
        result["rate_limits"] = rate_limits

    error_details: dict[str, Any] = {}
    error_data = diagnostics.get("error_data")
    if isinstance(error_data, dict):
        error_details.update(error_data)
    message = error_message or diagnostics.get("error_message")
    if isinstance(message, str) and message:
        error_details["message"] = message
    if returncode is not None:
        error_details["returncode"] = returncode
    if stderr:
        error_details["stderr_tail"] = stderr[-4000:]
    if error_details:
        result["error_details"] = error_details
    return result


def _merge_result_diagnostics(event: AgentEvent, diagnostics: dict[str, Any]) -> AgentEvent:
    additions = _result_diagnostics(diagnostics)
    if not additions:
        return event
    merged = dict(event)
    for key, value in additions.items():
        if key not in merged or merged[key] in (None, {}, []):
            merged[key] = value
    return merged


def _should_emit_event(
    event: AgentEvent,
    seen_assistant_texts: set[str],
    seen_result: dict[str, bool],
) -> bool:
    if event.get("type") == "result":
        if seen_result.get("emitted"):
            return False
        seen_result["emitted"] = True
        return True

    if event.get("type") != "assistant_text":
        return True
    text = event.get("text")
    if not isinstance(text, str) or not text:
        return True
    if text in seen_assistant_texts:
        return False
    seen_assistant_texts.add(text)
    return True


def _is_terminal_result(event: AgentEvent) -> bool:
    return event.get("type") == "result" and event.get("terminal") is True


def _stream_limit_message(stream: str, e: BaseException) -> str:
    consumed = getattr(e, "consumed", None)
    size_detail = f" before byte {consumed}" if isinstance(consumed, int) else ""
    limit = _format_stream_limit(CODEX_STREAM_LIMIT)
    return (
        f"codex {stream} emitted a single line larger than the {limit} "
        f"stream limit{size_detail}. This usually means one tool result, diff, "
        "or assistant event was too large to process as one JSONL record."
    )


def _format_stream_limit(limit: int) -> str:
    if limit >= 1024 * 1024 and limit % (1024 * 1024) == 0:
        return f"{limit // (1024 * 1024)} MiB"
    if limit >= 1024 and limit % 1024 == 0:
        return f"{limit // 1024} KiB"
    return f"{limit} bytes"


def _normalize_rate_limits(rate_limits: dict[str, Any]) -> dict[str, Any]:
    normalized = dict(rate_limits)
    for key in ("primary", "secondary"):
        window = normalized.get(key)
        if isinstance(window, dict):
            normalized[key] = _normalize_rate_limit_window(window)
    return normalized


def _normalize_rate_limit_window(window: dict[str, Any]) -> dict[str, Any]:
    normalized = dict(window)
    resets_at = normalized.get("resets_at")
    if isinstance(resets_at, (int, float)):
        normalized["resets_at_iso"] = dt.datetime.fromtimestamp(
            resets_at,
            tz=dt.timezone.utc,
        ).isoformat()
    return normalized
