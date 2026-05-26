from __future__ import annotations

from claude_agent_sdk import (
    AssistantMessage,
    ResultMessage,
    SystemMessage,
    TextBlock,
    ThinkingBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from agent_manager.providers.claude import translate_claude_message


def test_translate_claude_assistant_blocks() -> None:
    msg = AssistantMessage(
        content=[
            TextBlock("hello"),
            ThinkingBlock("thinking", "sig"),
            ToolUseBlock("tool-1", "Bash", {"command": "pwd"}),
        ],
        model="claude-test",
    )

    assert translate_claude_message(msg) == [
        {"type": "assistant_text", "text": "hello"},
        {"type": "thinking", "text": "thinking"},
        {"type": "tool_use", "id": "tool-1", "name": "Bash", "input": {"command": "pwd"}},
    ]


def test_translate_claude_tool_result() -> None:
    msg = UserMessage(content=[ToolResultBlock("tool-1", "ok", False)])

    assert translate_claude_message(msg) == [
        {"type": "tool_result", "tool_id": "tool-1", "output": "ok", "is_error": False}
    ]


def test_translate_claude_result_and_system_init() -> None:
    result = ResultMessage(
        subtype="success",
        duration_ms=10,
        duration_api_ms=9,
        is_error=False,
        num_turns=1,
        session_id="session-1",
        total_cost_usd=0.01,
        usage={"input_tokens": 1},
    )
    system = SystemMessage(subtype="init", data={"session_id": "session-1", "model": "claude-test"})

    assert translate_claude_message(result) == [
        {
            "type": "result",
            "subtype": "success",
            "duration_ms": 10,
            "num_turns": 1,
            "total_cost_usd": 0.01,
            "is_error": False,
            "session_id": "session-1",
            "usage": {"input_tokens": 1},
        }
    ]
    assert translate_claude_message(system) == [
        {"type": "system_init", "data": {"session_id": "session-1", "model": "claude-test"}}
    ]

