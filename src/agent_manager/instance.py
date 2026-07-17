from __future__ import annotations

import asyncio
import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Awaitable, Callable

from .artifacts import artifact_path_is_present, extract_artifact_directives
from .auth import classify_runtime_auth_failure
from .commands import handle_agent_command, parse_agent_command
from .providers.base import AgentConfig, AgentEvent, AgentInput, AgentRuntime, MemoryFileTracker
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
    # Memory file - contents prepended to every prompt
    memory_file: str | None = None

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
        # Continuous event pump architecture:
        #
        #   Inbox loop (this method)         Event pump (background task)
        #   ┌─────────────────────────┐      ┌────────────────────────────┐
        #   │ 1. wait turn_complete   │      │ async for e in runtime     │
        #   │ 2. read inbox           │      │        .event_stream():    │
        #   │ 3. runtime.query(...)   │────▶ │   publish(e)               │
        #   │ 4. loop                 │      │   if e.type == "result":   │
        #   │                         │      │       set turn_complete    │
        #   └─────────────────────────┘      └────────────────────────────┘
        #
        # The pump runs continuously — any events emitted by the provider (even
        # AFTER a turn's ResultMessage, from async task completions) get published
        # to the UI in real time. Turn boundaries are driven by "result" events,
        # not by iterator termination, so those out-of-band events no longer
        # queue until the next user prompt.
        runtime = self._create_runtime()
        self._runtime = runtime
        # Track the memory file's hash so we can detect edits between turns.
        # Claude embeds the memory in its system prompt at SDK startup, so
        # picking up edits requires restarting the runtime. Codex re-reads the
        # file on every turn anyway (fresh subprocess), but the restart is
        # cheap and keeps both providers on the same code path.
        memory_tracker = MemoryFileTracker(self.memory_file)

        # turn_complete is set() when no turn is active — initially yes.
        # Cleared before each query(), set again when the pump sees the first
        # "result" event for the turn (which releases the inbox loop to accept
        # the next user message — status can still be "running" if async work
        # is pending; see below).
        turn_complete = asyncio.Event()
        turn_complete.set()

        # Tool-call bookkeeping for the "stay running through async work" rule.
        # A "result" event with entries still open means we KNOW more events
        # (a tool_result, then more assistant_text, then another result) are
        # coming — hold status as "running" until those resolve.
        open_tool_ids: set[str] = set()

        # Event types that are informational only — receiving one of these
        # while status is "ready" does NOT mean the agent has resumed working.
        # (Everything else does — Rule 1: any content-shaped event during
        # ready implies async continuation → flip status back to "running".)
        INFORMATIONAL_TYPES = {
            "status",
            "user_prompt",
            "error",
            "auth_error",
            "aborted",
        }

        pump_task: asyncio.Task | None = None

        async def event_pump(rt: AgentRuntime) -> None:
            """Continuously publish events from the runtime's stream.

            Two status-transition rules layered on top of publishing:

            - Rule 1 (async continuations): if a content-shaped event arrives
              while we're in "ready", flip status back to "running" BEFORE
              publishing so the UI shows activity before the content lands.
            - Rule 2 (open tool calls): on a "result" event, only transition
              to "ready" if no tool_use is still awaiting its tool_result —
              otherwise stay "running" until every open tool closes and the
              next "result" arrives.
            """
            try:
                async for event in rt.event_stream():
                    etype = event.get("type")

                    # Rule 1: promote status BEFORE publishing so the status
                    # change appears in the event stream just ahead of the
                    # content event that triggered it.
                    if (
                        etype != "result"
                        and etype not in INFORMATIONAL_TYPES
                        and self.status == "ready"
                    ):
                        await self._set_status("running")

                    # Track tool-call lifecycle for Rule 2.
                    if etype == "tool_use":
                        tid = event.get("id")
                        if isinstance(tid, str):
                            open_tool_ids.add(tid)
                    elif etype == "tool_result":
                        tid = event.get("tool_id")
                        if isinstance(tid, str):
                            open_tool_ids.discard(tid)

                    await self._publish(event)

                    if etype == "result":
                        # Always release the inbox loop on the first result of a
                        # turn — the user can send the next prompt whenever
                        # they want, even if background tool work is still
                        # pending from this turn.
                        turn_complete.set()
                        # Only flip to "ready" if nothing is still expected.
                        # If tool calls (esp. async sub-agents) are open, we
                        # know more events are coming; keep "running".
                        if not open_tool_ids:
                            await self._set_status("ready")
            except asyncio.CancelledError:
                raise
            except Exception:
                log.exception("instance %s: event pump crashed", self.title)

        try:
            await runtime.start()
            pump_task = asyncio.create_task(
                event_pump(runtime), name=f"pump:{self.title}",
            )
            await self._set_status("ready")

            while True:
                # Wait for prior turn to finish before dispatching the next one.
                # (Prevents concurrent queries against the provider.)
                await turn_complete.wait()

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
                # Detect memory-file edits before dispatching the turn. If the
                # contents changed since last check, tear down the runtime and
                # rebuild it — _create_runtime() picks up the latest memory_file
                # path AND the latest captured session_id from self, so the new
                # client resumes the same conversation with the updated memory.
                if memory_tracker.refresh():
                    log.info(
                        "instance %s: memory file changed, restarting runtime",
                        self.title,
                    )
                    await self._publish({
                        "type": "status",
                        "status": "reloading_memory",
                    })
                    # Cancel pump before closing runtime — the pump's async-for
                    # against event_stream() would otherwise dangle on the queue.
                    if pump_task and not pump_task.done():
                        pump_task.cancel()
                        try:
                            await pump_task
                        except asyncio.CancelledError:
                            pass
                    try:
                        await runtime.close()
                    except Exception:
                        log.exception(
                            "instance %s: error closing runtime for memory reload",
                            self.title,
                        )
                    # Reset tool-call bookkeeping — the old runtime's tool IDs
                    # will never be closed by the new one.
                    open_tool_ids.clear()
                    runtime = self._create_runtime()
                    self._runtime = runtime
                    await runtime.start()
                    pump_task = asyncio.create_task(
                        event_pump(runtime), name=f"pump:{self.title}",
                    )
                turn_complete.clear()
                await self._set_status("running")
                await runtime.query(agent_input)
                # Loop back; pump will set turn_complete when it sees "result".
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.exception("instance %s crashed", self.title)
            message = f"{type(e).__name__}: {e}"
            await self._set_status("error")
            await self._publish({"type": "error", "message": message})
            # If the failure looks like rejected credentials (401/expired token)
            # or an upstream gateway error, surface a dedicated auth_error so the
            # registry can flag the provider and the UI can prompt re-auth — the
            # CLI's own auth status cannot detect this.
            reason = classify_runtime_auth_failure(message)
            if reason is not None:
                await self._publish({
                    "type": "auth_error",
                    "provider": self.provider,
                    "reason": reason,
                    "message": message,
                })
        finally:
            if pump_task and not pump_task.done():
                pump_task.cancel()
                try:
                    await pump_task
                except asyncio.CancelledError:
                    pass
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
            memory_file=self.memory_file,
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
