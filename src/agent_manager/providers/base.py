from __future__ import annotations

from collections.abc import AsyncIterator
from dataclasses import dataclass, field
from typing import Any, Protocol

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


class AgentRuntime(Protocol):
    provider: str

    async def start(self) -> None:
        """Prepare the provider runtime for turns."""

    async def run_turn(self, message: AgentInput) -> AsyncIterator[AgentEvent]:
        """Send one user input and yield normalized agent-manager events."""
        if False:
            yield {}

    async def close(self) -> None:
        """Release provider resources."""

