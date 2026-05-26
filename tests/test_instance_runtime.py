from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

import pytest

from agent_manager.instance import Instance
from agent_manager.providers.base import AgentConfig, AgentEvent, AgentInput


class FakeRuntime:
    provider = "codex"

    def __init__(self, config: AgentConfig) -> None:
        self.config = config
        self.started = False
        self.closed = False
        self.inputs: list[AgentInput] = []

    async def start(self) -> None:
        self.started = True

    async def run_turn(self, message: AgentInput) -> AsyncIterator[AgentEvent]:
        self.inputs.append(message)
        yield {"type": "system_init", "data": {"session_id": "fake-session", "model": "fake-model"}}
        yield {"type": "assistant_text", "text": f"echo: {message.text}"}
        yield {"type": "result", "session_id": "fake-session", "is_error": False}

    async def close(self) -> None:
        self.closed = True


async def _wait_for(predicate, timeout: float = 1.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while not predicate():
        if asyncio.get_running_loop().time() > deadline:
            raise AssertionError("timed out waiting for condition")
        await asyncio.sleep(0.01)


@pytest.mark.asyncio
async def test_instance_lifecycle_uses_provider_runtime_without_provider_specific_code() -> None:
    runtimes: list[FakeRuntime] = []

    def factory(config: AgentConfig) -> FakeRuntime:
        runtime = FakeRuntime(config)
        runtimes.append(runtime)
        return runtime

    events: list[AgentEvent] = []
    state_changes = 0

    async def on_event(event: AgentEvent) -> None:
        events.append(event)

    async def on_state_change() -> None:
        nonlocal state_changes
        state_changes += 1

    inst = Instance(
        title="future",
        path="/tmp/future",
        provider="codex",
        permission_mode="acceptEdits",
        _runtime_factory=factory,
        _on_event=on_event,
        _on_state_change=on_state_change,
    )
    subscriber = inst.subscribe()

    await inst.start()
    await _wait_for(lambda: inst.status == "ready")
    await inst.send("hello", images=[{"media_type": "image/png", "data": "abc"}])
    await _wait_for(lambda: any(e.get("type") == "result" for e in inst.history()))

    assert len(runtimes) == 1
    runtime = runtimes[0]
    assert runtime.started is True
    assert runtime.config.provider == "codex"
    assert runtime.config.cwd == "/tmp/future"
    assert runtime.inputs == [AgentInput("hello", [{"media_type": "image/png", "data": "abc"}])]

    assert inst.status == "ready"
    assert inst.session_id == "fake-session"
    assert inst.model == "fake-model"
    assert state_changes >= 1

    history_types = [event["type"] for event in inst.history()]
    assert history_types == [
        "status",
        "user_prompt",
        "status",
        "system_init",
        "assistant_text",
        "result",
        "status",
    ]
    assert inst.history()[1]["images"] == [{"media_type": "image/png", "data": "abc"}]
    assert [event["seq"] for event in inst.history()] == list(range(len(inst.history())))
    assert events == inst.history()

    subscriber_events: list[AgentEvent] = []
    while not subscriber.empty():
        subscriber_events.append(subscriber.get_nowait())
    assert [event["type"] for event in subscriber_events] == history_types

    await inst.stop()
    assert runtime.closed is True


@pytest.mark.asyncio
async def test_instance_slash_command_does_not_call_provider_runtime() -> None:
    runtimes: list[FakeRuntime] = []

    def factory(config: AgentConfig) -> FakeRuntime:
        runtime = FakeRuntime(config)
        runtimes.append(runtime)
        return runtime

    inst = Instance(
        title="commands",
        path="/tmp/commands",
        provider="codex",
        session_id="session-1",
        _runtime_factory=factory,
    )

    await inst.start()
    await _wait_for(lambda: inst.status == "ready")
    await inst.send("/unknown")
    await _wait_for(lambda: any(e.get("type") == "command_result" for e in inst.history()))

    assert len(runtimes) == 1
    assert runtimes[0].inputs == []
    history_types = [event["type"] for event in inst.history()]
    assert history_types == ["status", "user_prompt", "command_result"]
    assert inst.history()[-1]["is_error"] is True
    assert inst.status == "ready"

    await inst.stop()
