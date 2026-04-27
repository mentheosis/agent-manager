from __future__ import annotations

import asyncio
import datetime as dt
import logging
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    ResultMessage,
    SystemMessage,
    TextBlock,
    ThinkingBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

log = logging.getLogger(__name__)

Event = dict[str, Any]

HISTORY_CAP = 2000


@dataclass
class Instance:
    """One long-lived Claude Agent SDK session plus a pub/sub layer over its events."""

    title: str
    path: str
    permission_mode: str = "acceptEdits"
    status: str = "creating"
    created_at: str = ""
    display_title: str | None = None
    session_id: str | None = None

    _task: asyncio.Task | None = field(default=None, repr=False)
    _inbox: asyncio.Queue[str] = field(default_factory=asyncio.Queue, repr=False)
    _history: list[Event] = field(default_factory=list, repr=False)
    _subscribers: list[asyncio.Queue[Event]] = field(default_factory=list, repr=False)
    # Hooks injected by Registry. Both are awaitable; called with no arguments.
    _on_event: Callable[[Event], Awaitable[None]] | None = field(default=None, repr=False)
    _on_state_change: Callable[[], Awaitable[None]] | None = field(default=None, repr=False)

    async def start(self) -> None:
        self._task = asyncio.create_task(self._run(), name=f"instance:{self.title}")

    async def _run(self) -> None:
        opts: dict[str, Any] = {
            "cwd": self.path,
            "permission_mode": self.permission_mode,
        }
        if self.session_id:
            # Continue the prior conversation. CLI loads its persisted session jsonl.
            opts["resume"] = self.session_id
        options = ClaudeAgentOptions(**opts)
        try:
            async with ClaudeSDKClient(options=options) as client:
                await self._set_status("ready")
                while True:
                    prompt = await self._inbox.get()
                    await self._set_status("running")
                    await self._publish({"type": "user_prompt", "text": prompt})

                    await client.query(prompt)
                    async for msg in client.receive_response():
                        for event in self._translate(msg):
                            await self._publish(event)

                    await self._set_status("ready")
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.exception("instance %s crashed", self.title)
            await self._set_status("error")
            await self._publish({"type": "error", "message": f"{type(e).__name__}: {e}"})

    async def send(self, text: str) -> None:
        await self._inbox.put(text)

    async def stop(self) -> None:
        if self._task and not self._task.done():
            self._task.cancel()
            try:
                await self._task
            except asyncio.CancelledError:
                pass
        self.status = "deleted"

    def subscribe(self) -> asyncio.Queue[Event]:
        q: asyncio.Queue[Event] = asyncio.Queue()
        self._subscribers.append(q)
        return q

    def unsubscribe(self, q: asyncio.Queue[Event]) -> None:
        if q in self._subscribers:
            self._subscribers.remove(q)

    def history(self) -> list[Event]:
        return list(self._history)

    async def _set_status(self, status: str) -> None:
        self.status = status
        await self._publish({"type": "status", "status": status})

    async def _publish(self, event: Event) -> None:
        event.setdefault("ts", dt.datetime.now(dt.timezone.utc).isoformat())
        # Capture session_id from the SDK's init or result messages.
        session_changed = False
        sid = self._extract_session_id(event)
        if sid and sid != self.session_id:
            self.session_id = sid
            session_changed = True
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
        if session_changed and self._on_state_change is not None:
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

    def _translate(self, msg: Any) -> list[Event]:
        events: list[Event] = []
        if isinstance(msg, AssistantMessage):
            for block in msg.content:
                if isinstance(block, TextBlock):
                    events.append({"type": "assistant_text", "text": block.text})
                elif isinstance(block, ThinkingBlock):
                    events.append({"type": "thinking", "text": getattr(block, "thinking", "")})
                elif isinstance(block, ToolUseBlock):
                    events.append(
                        {
                            "type": "tool_use",
                            "id": block.id,
                            "name": block.name,
                            "input": block.input,
                        }
                    )
        elif isinstance(msg, UserMessage):
            content = msg.content
            if isinstance(content, list):
                for block in content:
                    if isinstance(block, ToolResultBlock):
                        output = block.content
                        if not isinstance(output, str):
                            output = str(output)
                        events.append(
                            {
                                "type": "tool_result",
                                "tool_id": block.tool_use_id,
                                "output": output,
                                "is_error": bool(getattr(block, "is_error", False)),
                            }
                        )
        elif isinstance(msg, ResultMessage):
            events.append(
                {
                    "type": "result",
                    "subtype": getattr(msg, "subtype", None),
                    "duration_ms": getattr(msg, "duration_ms", None),
                    "num_turns": getattr(msg, "num_turns", None),
                    "total_cost_usd": getattr(msg, "total_cost_usd", None),
                    "is_error": bool(getattr(msg, "is_error", False)),
                    "session_id": getattr(msg, "session_id", None),
                }
            )
        elif isinstance(msg, SystemMessage):
            subtype = getattr(msg, "subtype", None)
            if subtype == "init":
                events.append({"type": "system_init", "data": getattr(msg, "data", {})})
        return events
