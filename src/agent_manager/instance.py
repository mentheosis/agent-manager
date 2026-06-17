from __future__ import annotations

import asyncio
import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Awaitable, Callable

from .artifacts import artifact_path_is_present, extract_artifact_directives
from .commands import handle_agent_command, parse_agent_command
from .providers.base import AgentConfig, AgentEvent, AgentInput, AgentRuntime, MemoryFileTracker
from .providers.registry import RuntimeFactory, default_registry

from .session_manager import (
    DEFAULT_CONTEXT_WINDOW_SIZE,
    ContextMonitor,
    SessionConfig,
    SessionState,
)
from .handoff import HandoffCallbacks, HandoffCoordinator

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
    # Session management (optional, per-instance; disabled by default)
    session_config: SessionConfig = field(default_factory=SessionConfig)
    session_state: SessionState = field(default_factory=SessionState)

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
    # Session-management runtime (built lazily when session_config.enabled).
    _monitor: ContextMonitor | None = field(default=None, repr=False)
    _coordinator: HandoffCoordinator | None = field(default=None, repr=False)
    _handoff_was_active: bool = field(default=False, repr=False)

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
        self._ensure_session_mgmt()
        self._task = asyncio.create_task(self._run(), name=f"instance:{self.title}")

    async def _run(self) -> None:
        runtime = self._create_runtime()
        self._runtime = runtime
        # Track the memory file's hash so we can detect edits between turns.
        # Claude embeds the memory in its system prompt at SDK startup, so
        # picking up edits requires restarting the runtime. Codex re-reads the
        # file on every turn anyway (fresh subprocess), but the restart is
        # cheap and keeps both providers on the same code path.
        memory_tracker = MemoryFileTracker(self.memory_file)
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
                    try:
                        await runtime.close()
                    except Exception:
                        log.exception(
                            "instance %s: error closing runtime for memory reload",
                            self.title,
                        )
                    runtime = self._create_runtime()
                    self._runtime = runtime
                    await runtime.start()
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

    # --- session management -------------------------------------------------

    def _ensure_session_mgmt(self) -> None:
        """Build the ContextMonitor + HandoffCoordinator when session management is
        enabled. Idempotent; a no-op when disabled."""
        if not self.session_config.enabled or self._coordinator is not None:
            return
        self._monitor = ContextMonitor(self.session_config)
        # Restore the last persisted reading so the pressure bar shows the last known
        # occupancy immediately on (re)start, instead of 0% until the agent's first turn.
        self._monitor.seed_pressure(
            self.session_state.last_used_pct,
            self.session_state.last_total_tokens,
            self.session_state.last_window,
        )
        self._coordinator = HandoffCoordinator(
            self.title,
            self.session_config,
            self.session_state,
            HandoffCallbacks(
                send_prompt=self.send,
                respawn_fresh=self.restart_fresh,
                get_status=lambda: self.status,
                on_save=self._save_session_state,
            ),
        )

    def request_manual_split(self) -> bool:
        """Arm a manual split (Split button). Fires when the agent is next idle.
        Returns False if session management is not enabled."""
        self._ensure_session_mgmt()
        if self._monitor is None:
            return False
        self._monitor.request_manual_split()
        return True

    def session_info(self) -> dict[str, Any]:
        """Snapshot of session-management config/state/pressure for the API."""
        info: dict[str, Any] = {
            "enabled": self.session_config.enabled,
            "config": self.session_config.to_dict(),
            "state": self.session_state.to_dict(),
            "current_session": self.session_state.current_session,
        }
        if self._monitor is not None:
            info["pressure"] = self._monitor.pressure()
        if self._coordinator is not None:
            info["handoff_in_progress"] = self._coordinator.handoff_in_progress()
            st = self._coordinator.status()
            if st is not None:
                info["handoff"] = {"phase": int(st.phase), "trigger": st.trigger, "error": st.error}
        return info

    def set_session_config(self, config: SessionConfig) -> None:
        """Apply a new session config to a (possibly running) instance. These settings
        are in-process (not CLI flags), so no SDK restart is needed — unlike permission
        or model changes."""
        self.session_config = config
        if not config.enabled:
            # Stop triggering; an in-flight handoff (if any) finishes on its own.
            self._monitor = None
            self._coordinator = None
            return
        if self._monitor is None:
            self._coordinator = None  # force a clean rebuild from the new config
            self._ensure_session_mgmt()
        else:
            self._monitor.update_config(config)
            if self._coordinator is not None:
                self._coordinator.config = config

    def _resolve_window(self) -> int:
        """Best-effort context-window size for percentage math. Explicit config wins;
        otherwise infer from the model family, defaulting to 200K with a loud warning
        (a silent small default would trip thresholds ~5x too early on a 1M model)."""
        if self.session_config.context_window_size > 0:
            return self.session_config.context_window_size
        m = (self.model or "").lower()
        if "haiku" in m:
            return 200_000
        if "[1m]" in m or "opus-4" in m or "sonnet-4" in m or "fable" in m:
            return 1_000_000
        log.warning(
            "instance %s: unknown context window for model %r; defaulting to 200K", self.title, self.model
        )
        return DEFAULT_CONTEXT_WINDOW_SIZE

    async def _save_session_state(self, state: SessionState) -> None:
        self.session_state = state
        if self._on_state_change is not None:
            await self._on_state_change()

    async def _persist_pressure(self) -> None:
        """Persist the latest pressure reading onto session_state so it survives a
        restart (seeded back in _ensure_session_mgmt). Skips the disk write when the
        reading is unchanged — saving instances.json rewrites the whole file, so there
        is no point doing it for an identical consecutive reading."""
        mon = self._monitor
        if mon is None:
            return
        p = mon.pressure()
        pct = round(float(p.get("used_percentage", 0.0)), 1)
        total = int(p.get("total_context_tokens", 0))
        window = int(p.get("context_window_size", 0))
        if (
            pct == self.session_state.last_used_pct
            and total == self.session_state.last_total_tokens
            and window == self.session_state.last_window
        ):
            return
        self.session_state.last_used_pct = pct
        self.session_state.last_total_tokens = total
        self.session_state.last_window = window
        if self._on_state_change is not None:
            await self._on_state_change()

    async def _maybe_handle_session_boundary(self, status: str) -> None:
        """Event-driven analog of the Go pollMetadata idle gate. Resets the monitor
        once a handoff finishes (opening the cooldown), then — when idle and no handoff
        is running — fires the highest-precedence pending split."""
        coord, mon = self._coordinator, self._monitor
        if coord is None or mon is None:
            return
        in_progress = coord.handoff_in_progress()
        # Reset exactly once on the in-progress -> done edge (fresh session begun).
        if self._handoff_was_active and not in_progress:
            mon.reset()
            await self._persist_pressure()
            log.info("instance %s: handoff finished, monitor reset (cooldown open)", self.title)
        self._handoff_was_active = in_progress
        if in_progress or status != "ready":
            return
        trigger = mon.next_split_trigger()
        if trigger is None:
            return
        log.info("instance %s: triggering %s split (agent idle)", self.title, trigger)
        try:
            await coord.trigger_split(trigger)
        except Exception:
            log.exception("instance %s: failed to trigger split", self.title)

    async def restart_fresh(self) -> None:
        """Respawn the SDK client into a FRESH session (no resume) — the context-reset
        half of a split. Unlike reload_options (which preserves the conversation via
        resume), this clears session_id so the new session starts at ~0 tokens and
        rehydrates from the checkpoint the previous session wrote."""
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
            except Exception:
                log.exception("instance %s task ended with error during restart_fresh", self.title)
        self.session_id = None  # no resume -> fresh conversation
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
        await self._maybe_handle_session_boundary(status)

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
        # Feed LIVE context occupancy to the monitor from the per-turn assistant usage
        # (emitted by translate_claude_message in providers/claude.py). NOT the `result`
        # event — its usage is cumulative for the whole run and would climb past any
        # threshold on any window (the split-loop root cause;
        # see ROOT-CAUSE-cumulative-usage.md). Skip while a handoff is in progress so the
        # dying session's readings can't re-arm a split — the monitor is reset when the
        # handoff completes (see _maybe_handle_session_boundary).
        if (
            event.get("type") == "assistant_usage"
            and self._monitor is not None
            and (self._coordinator is None or not self._coordinator.handoff_in_progress())
        ):
            usage = event.get("usage")
            if isinstance(usage, dict):
                self._monitor.ingest_context_usage(usage, self._resolve_window())
                await self._persist_pressure()
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
