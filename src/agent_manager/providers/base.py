from __future__ import annotations

import asyncio
import hashlib
import logging
from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Protocol

from agent_manager.artifacts import artifact_instruction

log = logging.getLogger(__name__)

AgentEvent = dict[str, Any]


@dataclass(frozen=True)
class AgentInput:
    text: str
    images: list[dict[str, Any]] = field(default_factory=list)


@dataclass(frozen=True)
class AgentConfig:
    title: str
    provider: str
    cwd: str
    permission_mode: str = "acceptEdits"
    model: str | None = None
    session_id: str | None = None
    add_dirs: list[str] = field(default_factory=list)
    memory_file: str | None = None


class AgentRuntime(Protocol):
    """The provider-facing contract used by the Instance loop.

    The runtime is event-stream oriented — Instance sends messages via
    `query()` (non-blocking) and consumes events from `event_stream()` in a
    background pump. Turn boundaries are logical (driven by "result" events),
    not iteration boundaries. This lets providers like Claude, whose SDK can
    emit async events AFTER a turn's ResultMessage (from background tasks that
    completed later), stream those events immediately instead of queuing them
    until the next user prompt.

    `run_turn()` is provided as a convenience wrapper (`query()` + drain events
    until the first "result") mainly so tests can use request/response semantics
    against provider-specific runtimes without spinning up an Instance loop.
    Only one consumer of `event_stream()` may be active at a time — the Instance
    uses a background pump; tests use `run_turn()`; the two are not mixed.
    """

    provider: str

    async def start(self) -> None:
        """Prepare the runtime — open connections, spawn any background pumps."""

    async def query(self, message: AgentInput) -> None:
        """Send one message. Non-blocking — events arrive via event_stream().

        For Claude this dispatches a query to the persistent SDK client.
        For Codex this spawns a subprocess whose JSONL output is pumped into
        the event queue by a background task.
        """

    def event_stream(self) -> AsyncIterator[AgentEvent]:
        """Continuous async iterator of events for this runtime's lifetime.

        Yields events as they arrive, including any that come AFTER a "result"
        event (from async task completions on the provider side). The Instance
        drives one background pump on this stream and treats each "result"
        event as a logical turn boundary.
        """

    async def run_turn(self, message: AgentInput) -> AsyncIterator[AgentEvent]:
        """Convenience: send + drain events until the first "result".

        Provided for tests. The Instance loop does NOT use this — it drives
        query() and event_stream() directly.
        """
        if False:
            yield {}

    async def close(self) -> None:
        """Release provider resources — close connections, cancel pumps."""


class BaseRuntime:
    """Common runtime plumbing: shared event queue + `run_turn()` compatibility.

    Concrete providers subclass this and implement `start()`, `query()`, and
    `close()`. They push events into `self._event_queue` (via `_emit_event()`)
    from whatever background task reads their provider's stream. `event_stream()`
    and `run_turn()` are implemented once here so providers don't repeat them.
    """

    provider: str = ""

    def __init__(self) -> None:
        self._event_queue: asyncio.Queue[AgentEvent] = asyncio.Queue()

    async def _emit_event(self, event: AgentEvent) -> None:
        """Push an event onto the shared queue. Called by provider pumps."""
        await self._event_queue.put(event)

    async def event_stream(self) -> AsyncIterator[AgentEvent]:
        """Drain events from the shared queue forever (until cancelled)."""
        while True:
            event = await self._event_queue.get()
            yield event

    async def run_turn(self, message: AgentInput) -> AsyncIterator[AgentEvent]:
        """Compatibility helper — send and drain until the first "result" event."""
        await self.query(message)  # type: ignore[attr-defined]
        while True:
            event = await self._event_queue.get()
            yield event
            if event.get("type") == "result":
                return

    async def query(self, message: AgentInput) -> None:
        raise NotImplementedError

    async def start(self) -> None:
        raise NotImplementedError

    async def close(self) -> None:
        raise NotImplementedError


def read_memory_file(memory_file: str | None) -> str | None:
    """Read the contents of a memory file if it exists.

    Returns the file contents stripped of leading/trailing whitespace,
    or None if the file doesn't exist, is empty, or can't be read.
    """
    if not memory_file:
        return None
    try:
        path = Path(memory_file).expanduser().resolve()
        if path.is_file():
            content = path.read_text(encoding="utf-8").strip()
            if content:
                return content
    except (OSError, PermissionError) as e:
        log.warning("failed to read memory file %s: %s", memory_file, e)
    return None


def memory_file_hash(memory_file: str | None) -> str | None:
    """Compute a stable SHA-256 hex digest of the memory file's contents.

    Returns None when the file is unset, missing, or empty. Used to detect when
    the user edited the file so providers whose memory injection is set up once
    (e.g. Claude's system prompt) can reload to pick up the change.
    """
    content = read_memory_file(memory_file)
    if content is None:
        return None
    return hashlib.sha256(content.encode("utf-8")).hexdigest()


class MemoryFileTracker:
    """Tracks the hash of a memory file's contents to detect edits.

    Providers whose memory injection is set up ONCE (Claude — system prompt
    embedded in the SDK client at start) need this to know when the user
    edited the file so they can reload. Providers that re-read the file on
    every turn (Codex — fresh subprocess per turn) don't need it but harm
    nothing by using it.

    Centralizing the hash/compare logic here keeps providers free of
    duplicated file-watching code; they (or the Instance loop) just call
    `refresh()` before dispatching each turn.
    """

    def __init__(self, memory_file: str | None) -> None:
        self.memory_file = memory_file
        self._hash = memory_file_hash(memory_file)

    @property
    def current_hash(self) -> str | None:
        """The hash we last observed; updated on every refresh()."""
        return self._hash

    def has_changed(self) -> bool:
        """Return True if the file contents differ from the last observed hash.

        Does NOT update the stored hash — use refresh() for a check-and-update.
        """
        return memory_file_hash(self.memory_file) != self._hash

    def refresh(self) -> bool:
        """Re-hash the file. Returns True if it changed, then updates the stored hash.

        This is the normal entry point — callers want both the change signal
        and the side effect of "we've now acknowledged the new state".
        """
        new_hash = memory_file_hash(self.memory_file)
        changed = new_hash != self._hash
        self._hash = new_hash
        return changed


def format_memory_block(memory_file: str | None) -> str | None:
    """Render the memory file as a `<memory>` block tagged with its source path.

    Including the path lets the model know where the content originated, so it
    can reference it when suggesting updates (e.g. "consider adding X to
    /path/to/memory.md").

    Returns None if no memory file is set, the file is missing, or it's empty.
    """
    memory_content = read_memory_file(memory_file)
    if not memory_content:
        return None
    # Use the resolved absolute path so the model sees the real location.
    try:
        resolved = str(Path(memory_file).expanduser().resolve())
    except (OSError, RuntimeError):
        resolved = memory_file
    return (
        f'<memory source="{resolved}">\n'
        "The following is persistent context the user has chosen to keep in front "
        "of you across turns. Treat it as ground truth for this project. You may "
        "suggest edits to this file when relevant new facts emerge.\n\n"
        f"{memory_content}\n"
        "</memory>"
    )


def build_prompt_with_context(
    user_text: str,
    memory_file: str | None = None,
    include_artifact_instruction: bool = True,
) -> str:
    """Build a prompt with memory context and artifact instructions prepended.

    Structure when memory is present:
        <memory source="...">
        [contents of memory file]
        </memory>

        [artifact instruction]

        [user's prompt]

    Structure when memory is absent:
        [artifact instruction]

        [user's prompt]

    Args:
        user_text: The user's actual prompt text.
        memory_file: Optional path to a memory file to prepend.
        include_artifact_instruction: Whether to include artifact protocol instruction.

    Returns:
        The assembled prompt string.
    """
    parts: list[str] = []

    memory_block = format_memory_block(memory_file)
    if memory_block:
        parts.append(memory_block)

    if include_artifact_instruction:
        parts.append(artifact_instruction().strip())

    parts.append(user_text)

    return "\n\n".join(parts)

