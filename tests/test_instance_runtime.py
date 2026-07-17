from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator

import pytest

from agent_manager.instance import Instance
from agent_manager.providers.base import AgentConfig, AgentEvent, AgentInput, BaseRuntime


class FakeRuntime(BaseRuntime):
    provider = "codex"

    def __init__(self, config: AgentConfig) -> None:
        super().__init__()
        self.config = config
        self.started = False
        self.closed = False
        self.inputs: list[AgentInput] = []

    async def start(self) -> None:
        self.started = True

    async def query(self, message: AgentInput) -> None:
        # Push a canonical turn's events onto the shared queue. BaseRuntime's
        # event_stream() drains them and the Instance pump publishes them.
        self.inputs.append(message)
        await self._emit_event({"type": "system_init", "data": {"session_id": "fake-session", "model": "fake-model"}})
        await self._emit_event({"type": "assistant_text", "text": f"echo: {message.text}"})
        await self._emit_event({"type": "result", "session_id": "fake-session", "is_error": False})

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


class AuthFailingRuntime(BaseRuntime):
    provider = "claude"

    def __init__(self, config: AgentConfig) -> None:
        super().__init__()
        self.config = config

    async def start(self) -> None:
        pass

    async def query(self, message: AgentInput) -> None:
        # Raising from query() surfaces via Instance's exception handler, which
        # emits the auth_error event for expired-credential-shaped exceptions.
        raise RuntimeError("401 Unauthorized: OAuth token has expired")

    async def close(self) -> None:
        pass


@pytest.mark.asyncio
async def test_turn_failure_publishes_auth_error_event() -> None:
    inst = Instance(
        title="expired",
        path="/tmp/expired",
        provider="claude",
        _runtime_factory=lambda config: AuthFailingRuntime(config),
    )

    await inst.start()
    await _wait_for(lambda: inst.status == "ready")
    await inst.send("hi")
    await _wait_for(lambda: any(e.get("type") == "auth_error" for e in inst.history()))

    auth_events = [e for e in inst.history() if e.get("type") == "auth_error"]
    assert len(auth_events) == 1
    assert auth_events[0]["provider"] == "claude"
    assert auth_events[0]["reason"] == "expired"
    # The generic error event is still emitted for the transcript.
    assert any(e.get("type") == "error" for e in inst.history())

    await inst.stop()


class AsyncContinuationRuntime(BaseRuntime):
    """Runtime that emits a "result" then, later, more assistant_text events.

    Models Claude's real behavior when the model kicks off an async task and
    returns a ResultMessage before the task's output arrives. Under the old
    per-turn iteration model, those late events would sit in the SDK's queue
    and burst out ahead of the next turn's events. With the continuous pump
    they should stream to the UI in real time.
    """

    provider = "claude"

    def __init__(self, config: AgentConfig) -> None:
        super().__init__()
        self.config = config
        self.query_count = 0

    async def start(self) -> None:
        pass

    async def query(self, message: AgentInput) -> None:
        self.query_count += 1
        n = self.query_count
        await self._emit_event({"type": "assistant_text", "text": f"turn{n} initial"})
        await self._emit_event({"type": "result", "session_id": "async-session", "is_error": False})
        # Simulate an async task completing AFTER the result. In real Claude,
        # this would be an event pushed by the SDK on receive_messages() long
        # after receive_response() would have terminated.
        async def late() -> None:
            await asyncio.sleep(0.02)
            await self._emit_event({
                "type": "assistant_text",
                "text": f"turn{n} async continuation",
            })
        asyncio.create_task(late())

    async def close(self) -> None:
        pass


@pytest.mark.asyncio
async def test_async_events_between_turns_stream_in_order() -> None:
    """Regression test for the between-turn async event batching bug.

    Before the continuous-pump refactor, events emitted after a turn's "result"
    were held until the NEXT run_turn was invoked, then bursted out ahead of
    the next turn's events. The pump architecture publishes them as they arrive.
    """
    inst = Instance(
        title="asynccont",
        path="/tmp/asynccont",
        provider="claude",
        _runtime_factory=AsyncContinuationRuntime,
    )

    try:
        await inst.start()
        await _wait_for(lambda: inst.status == "ready")

        # Turn 1
        await inst.send("first")
        await _wait_for(
            lambda: any(
                e.get("type") == "assistant_text"
                and e.get("text") == "turn1 async continuation"
                for e in inst.history()
            ),
            timeout=2.0,
        )

        # Turn 2 — the async continuation from turn 1 should ALREADY be in
        # history before turn 2's events land (not batched with them).
        types_before_turn2 = [e.get("type") for e in inst.history()]
        assert "turn1 async continuation" in [
            e.get("text") for e in inst.history() if e.get("type") == "assistant_text"
        ]

        await inst.send("second")
        await _wait_for(
            lambda: sum(
                1 for e in inst.history() if e.get("type") == "result"
            ) >= 2,
            timeout=2.0,
        )

        # The continuation event for turn 1 must appear BEFORE turn 2's
        # user_prompt (i.e. between the two turns), proving it wasn't queued.
        texts_and_prompts = [
            (i, e) for i, e in enumerate(inst.history())
            if e.get("type") in ("assistant_text", "user_prompt")
        ]
        # Find where turn 1's continuation is vs where turn 2's user_prompt is.
        continuation_idx = next(
            i for i, e in texts_and_prompts
            if e.get("type") == "assistant_text" and e.get("text") == "turn1 async continuation"
        )
        user_prompts = [i for i, e in texts_and_prompts if e.get("type") == "user_prompt"]
        assert len(user_prompts) == 2, "expected exactly two user_prompt events"
        assert continuation_idx < user_prompts[1], (
            "async continuation from turn 1 was batched behind turn 2's user_prompt"
        )
    finally:
        await inst.stop()


class ToolCallRuntime(BaseRuntime):
    """Runtime whose turn emits a tool_use, a result, then later a tool_result.

    Models Claude spawning an async sub-agent: the model returns a
    ResultMessage before the tool completes, then the tool_result arrives.
    """

    provider = "claude"

    def __init__(self, config: AgentConfig) -> None:
        super().__init__()
        self.config = config

    async def start(self) -> None:
        pass

    async def query(self, message: AgentInput) -> None:
        await self._emit_event({
            "type": "tool_use",
            "id": "tool-a",
            "name": "Task",
            "input": {"prompt": "do async work"},
        })
        await self._emit_event({
            "type": "result",
            "session_id": "tool-session",
            "is_error": False,
        })

        # Deliver the async tool_result later, followed by a fresh result.
        async def async_tail() -> None:
            await asyncio.sleep(0.02)
            await self._emit_event({
                "type": "tool_result",
                "tool_id": "tool-a",
                "output": "done",
                "is_error": False,
            })
            await self._emit_event({"type": "assistant_text", "text": "wrapping up"})
            await self._emit_event({
                "type": "result",
                "session_id": "tool-session",
                "is_error": False,
            })
        asyncio.create_task(async_tail())

    async def close(self) -> None:
        pass


@pytest.mark.asyncio
async def test_status_stays_running_while_tool_calls_are_open() -> None:
    """Rule 2: a 'result' with open tool_use IDs keeps status='running' until
    the tools close and a subsequent 'result' arrives."""
    inst = Instance(
        title="opentools",
        path="/tmp/opentools",
        provider="claude",
        _runtime_factory=ToolCallRuntime,
    )
    statuses: list[str] = []

    try:
        await inst.start()
        await _wait_for(lambda: inst.status == "ready")
        # Reset baseline: only track statuses AFTER we send the message.
        await inst.send("go")
        # Wait until the initial result event has been published (turn dispatched).
        await _wait_for(
            lambda: sum(1 for e in inst.history() if e.get("type") == "result") >= 1,
            timeout=1.0,
        )
        # Immediately after the first result: tool is still open, so status
        # must NOT be "ready" yet.
        assert inst.status == "running", (
            f"expected status=running with open tool_use; got {inst.status}"
        )
        # Wait for the async tail to close the tool and emit its second result.
        await _wait_for(
            lambda: sum(1 for e in inst.history() if e.get("type") == "result") >= 2,
            timeout=2.0,
        )
        # After both results and the tool_result: back to ready.
        assert inst.status == "ready"
    finally:
        await inst.stop()


class LateAssistantRuntime(BaseRuntime):
    """Turn ends cleanly (no open tools), then an assistant_text arrives later."""

    provider = "claude"

    def __init__(self, config: AgentConfig) -> None:
        super().__init__()
        self.config = config

    async def start(self) -> None:
        pass

    async def query(self, message: AgentInput) -> None:
        await self._emit_event({"type": "assistant_text", "text": "initial"})
        await self._emit_event({
            "type": "result",
            "session_id": "late-session",
            "is_error": False,
        })

        async def late() -> None:
            await asyncio.sleep(0.02)
            await self._emit_event({
                "type": "assistant_text",
                "text": "async continuation",
            })
            await asyncio.sleep(0.05)
            await self._emit_event({
                "type": "result",
                "session_id": "late-session",
                "is_error": False,
            })
        asyncio.create_task(late())

    async def close(self) -> None:
        pass


@pytest.mark.asyncio
async def test_ready_flips_back_to_running_on_late_event() -> None:
    """Rule 1: a content event during 'ready' flips status back to 'running'
    until the next 'result' event, since more events are on the way."""
    inst = Instance(
        title="lateassist",
        path="/tmp/lateassist",
        provider="claude",
        _runtime_factory=LateAssistantRuntime,
    )

    try:
        await inst.start()
        await _wait_for(lambda: inst.status == "ready")
        await inst.send("go")
        # Wait for the first result to hit — status should be ready (no open tools).
        await _wait_for(
            lambda: sum(1 for e in inst.history() if e.get("type") == "result") >= 1
            and inst.status == "ready",
            timeout=1.0,
        )
        # Wait for the late assistant_text to arrive — status must flip back.
        await _wait_for(
            lambda: any(
                e.get("type") == "assistant_text" and e.get("text") == "async continuation"
                for e in inst.history()
            ),
            timeout=1.0,
        )
        # Between the late assistant_text and the next result, status was "running".
        # We may have already landed on the next result by now — assert it's ready.
        await _wait_for(
            lambda: sum(1 for e in inst.history() if e.get("type") == "result") >= 2,
            timeout=1.0,
        )
        assert inst.status == "ready"

        # Check the status-event history shows the flip: ...ready...running...ready
        status_seq = [
            e.get("status") for e in inst.history() if e.get("type") == "status"
        ]
        # Expected transitions: creating, ready (startup), running (query), ready
        # (first result), running (late assistant_text), ready (second result).
        assert status_seq.count("running") >= 2, (
            f"expected at least two running transitions, got {status_seq}"
        )
    finally:
        await inst.stop()


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
