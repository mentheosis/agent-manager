"""Regression tests for the split-loop root cause.

The context monitor must be fed LIVE per-turn occupancy from ``AssistantMessage.usage``
(its prompt size ≈ current context-window occupancy, which plateaus at the true context
size), NOT the cumulative ``ResultMessage.usage`` (which re-counts the cached context on
every API call and grows without bound, silently tripping any split threshold on any
window). See .state/sessions/session_manager/ROOT-CAUSE-cumulative-usage.md.

These lock in ``translate_claude_message`` (providers/claude.py) emitting an
``assistant_usage`` event — the only event that drives ``ingest_context_usage``.
"""

from claude_agent_sdk import AssistantMessage, TextBlock

from agent_manager.providers.claude import translate_claude_message


def test_assistant_message_emits_per_turn_usage_event():
    usage = {
        "input_tokens": 100,
        "cache_creation_input_tokens": 0,
        "cache_read_input_tokens": 50,
        "output_tokens": 10,
    }
    msg = AssistantMessage(
        content=[TextBlock(text="hi")],
        model="claude-opus-4-8[1m]",
        usage=usage,
    )
    events = translate_claude_message(msg)
    usage_events = [e for e in events if e.get("type") == "assistant_usage"]
    assert len(usage_events) == 1, "per-turn usage must surface exactly one event"
    assert usage_events[0]["usage"] == usage, "the raw per-turn usage must be passed through"


def test_assistant_message_without_usage_emits_no_usage_event():
    msg = AssistantMessage(content=[TextBlock(text="hi")], model="m", usage=None)
    events = translate_claude_message(msg)
    assert not any(e.get("type") == "assistant_usage" for e in events)


if __name__ == "__main__":
    test_assistant_message_emits_per_turn_usage_event()
    test_assistant_message_without_usage_emits_no_usage_event()
    print("ok: per-turn usage regression tests passed")
