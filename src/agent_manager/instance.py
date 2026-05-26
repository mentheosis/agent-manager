from __future__ import annotations

import asyncio
import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Awaitable, Callable

from .artifacts import artifact_path_is_present, extract_artifact_directives
from .commands import handle_agent_command, parse_agent_command
from .providers.base import AgentConfig, AgentEvent, AgentInput, AgentRuntime
from .providers.registry import RuntimeFactory, default_registry

log = logging.getLogger(__name__)

Event = AgentEvent
HISTORY_CAP = 2000


@dataclass
class Instance:
    """One provider-backed coding agent session plus a pub/sub event layer."""

    title: str
    path: str
    provider: str = "claude"  # "claude" now; "codex" once the runtime adapter exists
    kind: str = "agent"  # "agent" | "loop"
    permission_mode: str = "acceptEdits"
    model: str | None = None
    status: str = "creating"
    created_at: str = ""
    display_title: str | None = None
    session_id: str | None = None
    add_dirs: list[str] = field(default_factory=list)
    # Orchestration fields
    instance_type: str | None = None  # Compatibility alias: provider for agents, "loop" for teams
    parent: str | None = None  # Title of parent loop instance
    children: list[str] = field(default_factory=list)  # Titles of child instances
    agent_preset: str | None = None  # "coder" | "researcher" | "orchestrator"
    task: str | None = None  # Task description for loop instances
    # Organization
    folder: str | None = None  # Folder name for grouping in sidebar

    _task: asyncio.Task | None = field(default=None, repr=False)
    _inbox: asyncio.Queue[str | dict] = field(default_factory=asyncio.Queue, repr=False)
    _history: list[Event] = field(default_factory=list, repr=False)
    _next_seq: int = field(default=0, repr=False)  # Monotonic counter stamped on every event
    _subscribers: list[asyncio.Queue[Event]] = field(default_factory=list, repr=False)
    _runtime_factory: RuntimeFactory | None = field(default=None, repr=False)
    _runtime: AgentRuntime | None = field(default=None, repr=False)
    _current_turn_artifact_ids: set[str] = field(default_factory=set, repr=False)
    # Hooks injected by Registry. Both are awaitable; called with no arguments.
    _on_event: Callable[[Event], Awaitable[None]] | None = field(default=None, repr=False)
    _on_state_change: Callable[[], Awaitable[None]] | None = field(default=None, repr=False)

    def __post_init__(self) -> None:
        # Preserve the old "instance_type" contract while new code uses
        # provider/kind. Old records used instance_type="claude" for agents.
        if self.instance_type == "loop":
            self.kind = "loop"
        elif self.instance_type in ("claude", "codex"):
            self.provider = self.instance_type
            self.kind = "agent"
        self.sync_instance_type()

    def sync_instance_type(self) -> None:
        self.instance_type = "loop" if self.kind == "loop" else self.provider

    async def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name=f"instance:{self.title}")

    async def _run(self) -> None:
        runtime = self._create_runtime()
        self._runtime = runtime
        try:
            await runtime.start()
            await self._set_status("ready")
            while True:
                message = await self._inbox.get()
                log.info(
                    "instance %s: received message from inbox (type=%s)",
                    self.title,
                    type(message).__name__,
                )
                agent_input = self._normalize_input(message)
                await self._publish_user_prompt(agent_input)
                command = parse_agent_command(agent_input)
                if command is not None:
                    for event in handle_agent_command(
                        command,
                        provider=self.provider,
                        session_id=self.session_id,
                        has_images=bool(agent_input.images),
                    ):
                        await self._publish(event)
                    continue
                await self._set_status("running")
                async for event in runtime.run_turn(agent_input):
                    await self._publish(event)
                await self._set_status("ready")
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.exception("instance %s crashed", self.title)
            await self._set_status("error")
            await self._publish({"type": "error", "message": f"{type(e).__name__}: {e}"})
        finally:
            try:
                await runtime.close()
            except Exception:
                log.exception("instance %s runtime close failed", self.title)
            if self._runtime is runtime:
                self._runtime = None

    def _create_runtime(self) -> AgentRuntime:
        config = AgentConfig(
            title=self.title,
            provider=self.provider,
            cwd=self.path,
            permission_mode=self.permission_mode,
            model=self.model,
            session_id=self.session_id,
            add_dirs=list(self.add_dirs or []),
        )
        if self._runtime_factory is not None:
            return self._runtime_factory(config)
        return default_registry.create_runtime(config)

    @staticmethod
    def _normalize_input(message: str | dict) -> AgentInput:
        if isinstance(message, dict):
            return AgentInput(
                text=str(message.get("text", "")),
                images=list(message.get("images") or []),
            )
        return AgentInput(text=message)

    async def _publish_user_prompt(self, message: AgentInput) -> None:
        event: Event = {"type": "user_prompt", "text": message.text}
        if message.images:
            event["images"] = message.images
        await self._publish(event)

    async def send(self, text: str, images: list[dict] | None = None) -> None:
        """Send a message to the instance.

        Args:
            text: The text message
            images: Optional list of images, each with {media_type, data} where
                    data is base64-encoded image content
        """
        if images:
            # Multimodal message - pass as dict
            await self._inbox.put({"text": text, "images": images})
        else:
            # Simple text message
            await self._inbox.put(text)

    async def stop(self) -> None:
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        self.status = "deleted"

    async def abort(self) -> None:
        """Abort the current operation and restart the SDK client.

        This cancels any in-progress query but keeps the instance alive,
        ready to accept new prompts. The conversation is preserved via session_id.
        """
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            except Exception:
                log.exception("instance %s task ended with error during abort", self.title)
        await self._publish({"type": "aborted", "message": "Operation cancelled by user"})
        await self._set_status("creating")
        self._task = asyncio.create_task(self._run(), name=f"instance:{self.title}")

    async def reload_options(self) -> None:
        """Tear down and restart the SDK client with the current options.

        Used after add_dirs / permission_mode changes — the CLI subprocess sees
        them as command-line flags at spawn, so a restart is required. The
        prior conversation is preserved because session_id is in self and gets
        passed as `resume=` to the new client.
        """
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            except Exception:
                log.exception("instance %s background task ended with error during reload", self.title)
        await self._set_status("creating")
        self._task = asyncio.create_task(self._run(), name=f"instance:{self.title}")

    def subscribe(self) -> asyncio.Queue[Event]:
        q: asyncio.Queue[Event] = asyncio.Queue()
        self._subscribers.append(q)
        return q

    def unsubscribe(self, q: asyncio.Queue[Event]) -> None:
        if q in self._subscribers:
            self._subscribers.remove(q)

    def history(self) -> list[Event]:
        return list(self._history)

    def debug_state(self) -> dict[str, object]:
        task = self._task
        runtime = self._runtime
        proc = getattr(runtime, "_proc", None) if runtime is not None else None
        subprocess_state = None
        if proc is not None:
            subprocess_state = {
                "pid": getattr(proc, "pid", None),
                "returncode": getattr(proc, "returncode", None),
            }

        last_event = self._history[-1] if self._history else None
        return {
            "status": self.status,
            "task": {
                "exists": task is not None,
                "done": bool(task.done()) if task is not None else None,
                "cancelled": bool(task.cancelled()) if task is not None else None,
            },
            "runtime": type(runtime).__name__ if runtime is not None else None,
            "subprocess": subprocess_state,
            "inbox_size": self._inbox.qsize(),
            "subscriber_count": len(self._subscribers),
            "history_length": len(self._history),
            "next_seq": self._next_seq,
            "last_event": _event_summary(last_event),
        }

    async def _set_status(self, status: str) -> None:
        self.status = status
        await self._publish({"type": "status", "status": status})

    async def _publish(self, event: Event) -> None:
        for expanded in self._expand_artifact_directives(event):
            await self._publish_one(expanded)

    def _expand_artifact_directives(self, event: Event) -> list[Event]:
        if event.get("type") == "assistant_text":
            text = event.get("text")
            if isinstance(text, str):
                cleaned, artifacts = extract_artifact_directives(text, source=self.provider)
                artifacts = self._valid_artifacts(artifacts)
                if artifacts:
                    updated = dict(event)
                    updated["text"] = cleaned
                    return [updated, *artifacts]
        if event.get("type") == "tool_result":
            output = event.get("output")
            if isinstance(output, str):
                cleaned, artifacts = extract_artifact_directives(output, source=self.provider)
                artifacts = self._valid_artifacts(artifacts)
                if artifacts:
                    updated = dict(event)
                    updated["output"] = cleaned
                    return [updated, *artifacts]
        return [event]

    @staticmethod
    def _valid_artifacts(artifacts: list[Event]) -> list[Event]:
        return [
            artifact
            for artifact in artifacts
            if isinstance(artifact.get("path"), str) and artifact_path_is_present(artifact["path"])
        ]

    async def _publish_one(self, event: Event) -> None:
        if event.get("type") == "user_prompt":
            self._current_turn_artifact_ids.clear()
        if event.get("type") == "artifact":
            artifact_id = event.get("artifact_id")
            if isinstance(artifact_id, str) and artifact_id:
                if artifact_id in self._current_turn_artifact_ids:
                    return
                self._current_turn_artifact_ids.add(artifact_id)
        event.setdefault("ts", dt.datetime.now(dt.timezone.utc).isoformat())
        # Stamp a monotonically-increasing sequence number so clients can
        # resume from a known position after a WebSocket reconnect.
        event["seq"] = self._next_seq
        self._next_seq += 1
        # Capture session_id from the SDK's init or result messages.
        state_changed = False
        sid = self._extract_session_id(event)
        if sid and sid != self.session_id:
            self.session_id = sid
            state_changed = True
        # Capture model from system_init if we don't have one configured.
        # This resolves "SDK default" to the actual model being used.
        if event.get("type") == "system_init" and not self.model:
            model = self._extract_model(event)
            if model:
                self.model = model
                state_changed = True
                log.info("instance %s: resolved default model to %s", self.title, model)
        self._history.append(event)
        if len(self._history) > HISTORY_CAP:
            del self._history[: len(self._history) - HISTORY_CAP]
        if self._on_event is not None:
            try:
                await self._on_event(event)
            except Exception:
                log.exception("on_event hook failed for %s", self.title)
        for q in list(self._subscribers):
            q.put_nowait(event)
        if state_changed and self._on_state_change is not None:
            try:
                await self._on_state_change()
            except Exception:
                log.exception("on_state_change hook failed for %s", self.title)

    @staticmethod
    def _extract_session_id(event: Event) -> str | None:
        if "session_id" in event and event["session_id"]:
            return event["session_id"]
        data = event.get("data")
        if isinstance(data, dict):
            sid = data.get("session_id")
            if sid:
                return sid
        return None

    @staticmethod
    def _extract_model(event: Event) -> str | None:
        """Extract model from system_init event data."""
        data = event.get("data")
        if isinstance(data, dict):
            model = data.get("model")
            if model:
                return model
        return None


def _event_summary(event: Event | None) -> dict[str, object] | None:
    if not event:
        return None
    summary: dict[str, object] = {
        "type": event.get("type"),
        "seq": event.get("seq"),
        "ts": event.get("ts"),
    }
    if "subtype" in event:
        summary["subtype"] = event.get("subtype")
    if "is_error" in event:
        summary["is_error"] = event.get("is_error")
    if "status" in event:
        summary["status"] = event.get("status")
    if "session_id" in event:
        summary["session_id"] = event.get("session_id")
    text = event.get("text") or event.get("message")
    if isinstance(text, str):
        summary["preview"] = text[:240]
    return summary
