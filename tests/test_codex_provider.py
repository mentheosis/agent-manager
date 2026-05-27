from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from agent_manager.artifacts import artifact_id_for_path
from agent_manager.providers.base import AgentConfig, AgentInput
from agent_manager.providers.codex import CodexRuntime, _normalize_rate_limits, _should_emit_event
from agent_manager.providers.codex_events import translate_codex_event, translate_codex_transcript_event
from agent_manager.providers.codex_metadata import (
    _parse_codex_doctor_metadata,
    _parse_codex_model_catalog,
)


def test_translate_codex_thread_started_and_result() -> None:
    assert translate_codex_event({"type": "thread.started", "id": "session-1", "model": "gpt-test"}) == [
        {"type": "system_init", "data": {"provider": "codex", "session_id": "session-1", "model": "gpt-test"}}
    ]
    assert translate_codex_event(
        {"type": "thread.started", "id": "session-1"},
        system_context={
            "active_model_label": "gpt-default",
            "sandbox": "workspace-write",
            "auth_mode": "chatgpt",
        },
    ) == [
        {
            "type": "system_init",
            "data": {
                "active_model_label": "gpt-default",
                "sandbox": "workspace-write",
                "auth_mode": "chatgpt",
                "provider": "codex",
                "session_id": "session-1",
            },
        }
    ]
    assert translate_codex_event({"type": "turn.completed", "session_id": "session-1", "usage": {"input_tokens": 1}}) == [
        {
            "type": "result",
            "subtype": "success",
            "duration_ms": None,
            "num_turns": None,
            "total_cost_usd": None,
            "estimated_cost_usd": None,
            "estimated_cost_model": None,
            "is_error": False,
            "session_id": "session-1",
            "usage": {"input_tokens": 1},
        }
    ]
    estimated = translate_codex_event(
        {
            "type": "turn.completed",
            "usage": {
                "input_tokens": 1000,
                "cached_input_tokens": 400,
                "output_tokens": 50,
                "reasoning_output_tokens": 25,
            },
        },
        system_context={"active_model_label": "gpt-5-codex"},
    )
    assert estimated[0]["estimated_cost_model"] == "gpt-5-codex"
    assert estimated[0]["estimated_cost_usd"] == pytest.approx(0.00155)


def test_translate_codex_assistant_and_tool_items(tmp_path: Path) -> None:
    assert translate_codex_event({
        "type": "item.completed",
        "item": {"type": "agent_message", "text": "hello"},
    }) == [{"type": "assistant_text", "text": "hello"}]

    assert translate_codex_event({
        "type": "item.started",
        "item_id": "cmd-1",
        "item": {"type": "command_execution", "command": "pwd"},
    }) == [{"type": "tool_use", "id": "cmd-1", "name": "command_execution", "input": "pwd"}]

    assert translate_codex_event({
        "type": "item.completed",
        "item_id": "cmd-1",
        "item": {"type": "command_execution", "output": "/repo"},
    }) == [{"type": "tool_result", "tool_id": "cmd-1", "output": "/repo", "is_error": False}]

    assert translate_codex_event({
        "type": "item.completed",
        "item": {
            "id": "item_1",
            "type": "command_execution",
            "command": "/bin/bash -lc 'printf hello'",
            "aggregated_output": "hello",
            "exit_code": 0,
            "status": "completed",
        },
    }) == [{"type": "tool_result", "tool_id": "item_1", "output": "hello", "is_error": False}]

    assert translate_codex_event({
        "type": "item.completed",
        "item": {
            "id": "item_2",
            "type": "command_execution",
            "command": "/bin/bash -lc false",
            "aggregated_output": "failed",
            "exit_code": 1,
            "status": "failed",
        },
    }) == [{"type": "tool_result", "tool_id": "item_2", "output": "failed", "is_error": True}]

    assert translate_codex_event({
        "type": "item.completed",
        "item_id": "cmd-2",
        "item": {
            "type": "function_call_output",
            "output": "bwrap: No permissions to create a new namespace",
        },
    }) == [
        {
            "type": "tool_result",
            "tool_id": "cmd-2",
            "output": "bwrap: No permissions to create a new namespace",
            "is_error": False,
        }
    ]

    assert translate_codex_event({
        "type": "item.failed",
        "item_id": "cmd-3",
        "item": {"type": "custom_tool_call", "error": {"message": "failed"}},
    }) == [{"type": "tool_result", "tool_id": "cmd-3", "output": "failed", "is_error": True}]

    assert translate_codex_event({
        "type": "item.started",
        "item_id": "search-1",
        "item": {"type": "web_search_call", "query": "current OpenAI API models"},
    }) == [
        {
            "type": "tool_use",
            "id": "search-1",
            "name": "web_search_call",
            "input": {"query": "current OpenAI API models"},
        }
    ]

    assert translate_codex_event({
        "type": "item.started",
        "item_id": "plan-1",
        "item": {
            "type": "function_call",
            "name": "update_plan",
            "arguments": {
                "explanation": "Checking the affected parser and renderer.",
                "plan": [
                    {"step": "Read event parser", "status": "completed"},
                    {"step": "Patch update_plan display", "status": "in_progress"},
                ],
            },
        },
    }) == [
        {
            "type": "tool_use",
            "id": "plan-1",
            "name": "update_plan",
            "input": {
                "explanation": "Checking the affected parser and renderer.",
                "plan": [
                    {"step": "Read event parser", "status": "completed"},
                    {"step": "Patch update_plan display", "status": "in_progress"},
                ],
            },
            "display_text": "Checking the affected parser and renderer.\nIn progress: Patch update_plan display",
        }
    ]

    assert translate_codex_event({
        "type": "item.started",
        "item_id": "plan-2",
        "item": {
            "type": "function_call",
            "name": "update_plan",
            "arguments": {
                "plan": [{"step": "Finish", "status": "completed"}],
            },
        },
    }) == [
        {
            "type": "tool_use",
            "id": "plan-2",
            "name": "update_plan",
            "input": {"plan": [{"step": "Finish", "status": "completed"}]},
            "display_text": "Plan updated.",
        }
    ]

    image = tmp_path / "example.png"
    image.write_bytes(b"\x89PNG\r\n\x1a\n")
    image_path = str(image)
    assert translate_codex_event({
        "type": "item.completed",
        "item_id": "image-1",
        "item": {"type": "image_generation", "output": f"saved to {image_path}"},
    }) == [
        {
            "type": "tool_result",
            "tool_id": "image-1",
            "output": f"saved to {image_path}",
            "is_error": False,
        },
        {
            "type": "artifact",
            "artifact_type": "image",
            "artifact_id": artifact_id_for_path(image_path),
            "path": image_path,
            "title": "example.png",
            "mime_type": "image/png",
            "source": "codex",
        },
    ]


def test_translate_codex_transcript_patch_events() -> None:
    assert translate_codex_transcript_event({
        "type": "response_item",
        "payload": {
            "type": "custom_tool_call",
            "call_id": "call-1",
            "name": "apply_patch",
            "input": "*** Begin Patch\n*** Update File: app.py\n*** End Patch\n",
        },
    }) == [
        {
            "type": "tool_use",
            "id": "call-1",
            "name": "apply_patch",
            "input": "*** Begin Patch\n*** Update File: app.py\n*** End Patch\n",
        }
    ]

    result = translate_codex_transcript_event({
        "type": "event_msg",
        "payload": {
            "type": "patch_apply_end",
            "call_id": "call-1",
            "stdout": "Success. Updated the following files:\nM app.py\n",
            "stderr": "",
            "success": True,
            "changes": {"/repo/app.py": {"type": "update", "unified_diff": "@@"}},
        },
    })
    assert result == [
        {
            "type": "tool_result",
            "tool_id": "call-1",
            "output": "Success. Updated the following files:\nM app.py\n\nChanged files:\nupdate: /repo/app.py",
            "is_error": False,
        }
    ]

    assert translate_codex_transcript_event({
        "type": "event_msg",
        "payload": {
            "type": "mcp_tool_call_end",
            "call_id": "call-2",
            "invocation": {
                "server": "codex",
                "tool": "list_mcp_resources",
                "arguments": {},
            },
            "result": {"Ok": {"content": [{"type": "text", "text": "{\"resources\":[]}"}]}},
        },
    }) == [
        {
            "type": "tool_use",
            "id": "call-2",
            "name": "mcp.list_mcp_resources",
            "input": {"server": "codex", "tool": "list_mcp_resources", "arguments": {}},
        },
        {
            "type": "tool_result",
            "tool_id": "call-2",
            "output": "{\"resources\":[]}",
            "is_error": False,
        },
    ]


def test_translate_codex_transcript_function_and_tool_search_events() -> None:
    assert translate_codex_transcript_event({
        "type": "response_item",
        "payload": {
            "type": "function_call",
            "name": "exec_command",
            "arguments": "{\"cmd\":\"python3 build.py\"}",
            "call_id": "call-exec",
        },
    }) == [
        {
            "type": "tool_use",
            "id": "call-exec",
            "name": "exec_command",
            "input": "{\"cmd\":\"python3 build.py\"}",
        }
    ]

    assert translate_codex_transcript_event({
        "type": "response_item",
        "payload": {
            "type": "function_call_output",
            "call_id": "call-exec",
            "output": "Process exited with code 0\nOutput:\nok",
        },
    }) == [
        {
            "type": "tool_result",
            "tool_id": "call-exec",
            "output": "Process exited with code 0\nOutput:\nok",
            "is_error": False,
        }
    ]

    assert translate_codex_transcript_event({
        "type": "response_item",
        "payload": {
            "type": "tool_search_call",
            "call_id": "call-search",
            "arguments": {"query": "docker mcp", "limit": 5},
        },
    }) == [
        {
            "type": "tool_use",
            "id": "call-search",
            "name": "tool_search_call",
            "input": {"query": "docker mcp", "limit": 5},
        }
    ]

    assert translate_codex_transcript_event({
        "type": "response_item",
        "payload": {
            "type": "function_call",
            "name": "update_plan",
            "arguments": json.dumps({
                "explanation": "Now wiring the terminal display.",
                "plan": [
                    {"step": "Parse plan summaries", "status": "completed"},
                    {"step": "Render summary text", "status": "in_progress"},
                ],
            }),
            "call_id": "call-plan",
        },
    }) == [
        {
            "type": "tool_use",
            "id": "call-plan",
            "name": "update_plan",
            "input": json.dumps({
                "explanation": "Now wiring the terminal display.",
                "plan": [
                    {"step": "Parse plan summaries", "status": "completed"},
                    {"step": "Render summary text", "status": "in_progress"},
                ],
            }),
            "display_text": "Now wiring the terminal display.\nIn progress: Render summary text",
        }
    ]

    assert translate_codex_transcript_event({
        "type": "response_item",
        "payload": {
            "type": "function_call",
            "name": "update_goal",
            "arguments": "{\"status\":\"complete\"}",
            "call_id": "call-goal",
        },
    }) == [
        {
            "type": "tool_use",
            "id": "call-goal",
            "name": "update_goal",
            "input": "{\"status\":\"complete\"}",
            "concludes_turn": True,
            "display_text": "Goal marked complete.",
        }
    ]


def test_translate_codex_transcript_final_agent_message_and_task_complete() -> None:
    assert translate_codex_transcript_event({
        "type": "event_msg",
        "payload": {
            "type": "agent_message",
            "message": "Finished the goal.",
            "phase": "final_answer",
        },
    }) == [{"type": "assistant_text", "text": "Finished the goal."}]

    assert translate_codex_transcript_event({
        "type": "event_msg",
        "payload": {
            "type": "task_complete",
            "duration_ms": 856271,
            "last_agent_message": "Finished the goal.",
        },
    }) == [
        {
            "type": "result",
            "subtype": "success",
            "duration_ms": 856271,
            "is_error": False,
            "terminal": True,
        }
    ]


def test_parse_codex_model_catalog_filters_to_visible_available_models() -> None:
    assert _parse_codex_model_catalog({
        "models": [
            {"slug": "hidden", "visibility": "hide", "priority": 0},
            {"slug": "upgrade-only", "visibility": "list", "upgrade": {"plan": "pro"}, "priority": 1},
            {"slug": "gpt-fast", "visibility": "list", "priority": 20},
            {"slug": "gpt-frontier", "visibility": "list", "priority": 10},
            {"slug": "gpt-fast", "visibility": "list", "priority": 30},
        ]
    }) == ["gpt-frontier", "gpt-fast"]


def test_parse_codex_doctor_metadata_is_sanitized() -> None:
    assert _parse_codex_doctor_metadata({
        "codexVersion": "0.133.0",
        "checks": {
            "config.load": {
                "details": {
                    "model": "gpt-5.5",
                    "model provider": "openai",
                    "config.toml": "/secret/path/config.toml",
                }
            },
            "auth.credentials": {
                "details": {
                    "stored auth mode": "chatgpt",
                    "auth file": "/secret/path/auth.json",
                }
            },
        },
    }) == {
        "cli_version": "0.133.0",
        "configured_model": "gpt-5.5",
        "active_model_label": "gpt-5.5",
        "model_provider": "openai",
        "auth_mode": "chatgpt",
    }


def test_codex_command_builder_fresh_and_resume() -> None:
    fresh = CodexRuntime(AgentConfig(
        title="new",
        provider="codex",
        cwd="/repo",
        permission_mode="workspace-write",
        model="gpt-test",
        add_dirs=["/other"],
    ))
    assert fresh._build_command("hi", []) == [
        "codex",
        "exec",
        "--json",
        "--cd",
        "/repo",
        "--sandbox",
        "workspace-write",
        "--skip-git-repo-check",
        "--model",
        "gpt-test",
        "--add-dir",
        "/other",
        "hi",
    ]

    resumed = CodexRuntime(AgentConfig(
        title="old",
        provider="codex",
        cwd="/repo",
        permission_mode="workspace-write",
        session_id="session-1",
    ))
    assert resumed._build_command("again", []) == [
        "codex",
        "exec",
        "resume",
        "--json",
        "--skip-git-repo-check",
        "session-1",
        "again",
    ]

    dangerous = CodexRuntime(AgentConfig(
        title="danger",
        provider="codex",
        cwd="/repo",
        permission_mode="danger-full-access",
        session_id="session-1",
    ))
    assert dangerous._build_command("again", []) == [
        "codex",
        "exec",
        "resume",
        "--json",
        "--skip-git-repo-check",
        "--dangerously-bypass-approvals-and-sandbox",
        "session-1",
        "again",
    ]


def test_normalize_rate_limits_adds_reset_iso() -> None:
    assert _normalize_rate_limits({
        "limit_id": "codex",
        "plan_type": "pro",
        "primary": {"used_percent": 11.0, "window_minutes": 300, "resets_at": 1779768973},
        "secondary": {"used_percent": 9.0, "window_minutes": 10080, "resets_at": 1780172139},
    }) == {
        "limit_id": "codex",
        "plan_type": "pro",
        "primary": {
            "used_percent": 11.0,
            "window_minutes": 300,
            "resets_at": 1779768973,
            "resets_at_iso": "2026-05-26T04:16:13+00:00",
        },
        "secondary": {
            "used_percent": 9.0,
            "window_minutes": 10080,
            "resets_at": 1780172139,
            "resets_at_iso": "2026-05-30T20:15:39+00:00",
        },
    }


def test_should_emit_event_dedupes_assistant_text_within_turn() -> None:
    seen: set[str] = set()
    seen_result = {"emitted": False}

    assert _should_emit_event({"type": "assistant_text", "text": "hello"}, seen, seen_result) is True
    assert _should_emit_event({"type": "assistant_text", "text": "hello"}, seen, seen_result) is False
    assert _should_emit_event({"type": "assistant_text", "text": "different"}, seen, seen_result) is True
    assert _should_emit_event({"type": "tool_use", "name": "update_plan"}, seen, seen_result) is True


def test_should_emit_event_dedupes_result_within_turn() -> None:
    seen: set[str] = set()
    seen_result = {"emitted": False}

    assert _should_emit_event({"type": "result", "subtype": "success"}, seen, seen_result) is True
    assert _should_emit_event({"type": "result", "subtype": "success"}, seen, seen_result) is False


@pytest.mark.asyncio
async def test_codex_system_context_includes_latest_rate_limits(tmp_path: Path, monkeypatch) -> None:
    monkeypatch.setenv("HOME", str(tmp_path))
    session_id = "session-rate-limit"
    session_file = tmp_path / ".codex" / "sessions" / "2026" / "05" / "26" / f"rollout-{session_id}.jsonl"
    session_file.parent.mkdir(parents=True)
    session_file.write_text(
        "\n".join([
            '{"timestamp":"2026-05-26T03:00:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"pro","primary":{"used_percent":5.0,"window_minutes":300,"resets_at":1779768973}}}}',
            '{"timestamp":"2026-05-26T03:05:00.000Z","type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":"codex","plan_type":"pro","primary":{"used_percent":6.0,"window_minutes":300,"resets_at":1779768973},"secondary":{"used_percent":9.0,"window_minutes":10080,"resets_at":1780172139}}}}',
        ]),
        encoding="utf-8",
    )
    runtime = CodexRuntime(AgentConfig(
        title="codex",
        provider="codex",
        cwd="/repo",
        permission_mode="workspace-write",
        model="gpt-test",
        session_id=session_id,
    ))

    context = await runtime._system_init_context()

    assert context["rate_limits"]["observed_at"] == "2026-05-26T03:05:00.000Z"
    assert context["rate_limits"]["primary"]["used_percent"] == 6.0
    assert context["rate_limits"]["primary"]["resets_at_iso"] == "2026-05-26T04:16:13+00:00"
    assert context["rate_limits"]["secondary"]["used_percent"] == 9.0


@pytest.mark.asyncio
async def test_codex_runtime_reads_jsonl_from_subprocess(tmp_path: Path, monkeypatch) -> None:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    fake_codex = bin_dir / "codex"
    fake_codex.write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' 'WARNING: benign startup diagnostic'\n"
        "printf '%s\\n' '{\"type\":\"thread.started\",\"id\":\"session-1\",\"model\":\"gpt-test\"}'\n"
        "printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"hello\"}}'\n"
        "printf '%s\\n' '{\"type\":\"turn.completed\",\"session_id\":\"session-1\"}'\n",
        encoding="utf-8",
    )
    fake_codex.chmod(0o755)
    monkeypatch.setenv("PATH", str(bin_dir) + os.pathsep + os.environ.get("PATH", ""))

    async def fake_metadata(cwd: str | None = None) -> dict[str, str]:
        return {"configured_model": "gpt-default", "active_model_label": "gpt-default", "auth_mode": "chatgpt"}

    monkeypatch.setattr("agent_manager.providers.codex.fetch_codex_runtime_metadata", fake_metadata)

    runtime = CodexRuntime(AgentConfig(
        title="codex",
        provider="codex",
        cwd=str(tmp_path),
        permission_mode="workspace-write",
    ))

    await runtime.start()
    events = [event async for event in runtime.run_turn(AgentInput("hello"))]

    assert events == [
        {
            "type": "system_init",
            "data": {
                "cwd": str(tmp_path),
                "permission_mode": "workspace-write",
                "sandbox": "workspace-write",
                "resume": False,
                "command": "codex exec",
                "configured_model": "gpt-default",
                "active_model_label": "gpt-default",
                "auth_mode": "chatgpt",
                "provider": "codex",
                "session_id": "session-1",
                "model": "gpt-test",
            },
        },
        {"type": "assistant_text", "text": "hello"},
        {
            "type": "result",
            "subtype": "success",
            "duration_ms": None,
                "num_turns": None,
                "total_cost_usd": None,
                "estimated_cost_usd": None,
                "estimated_cost_model": None,
                "is_error": False,
                "session_id": "session-1",
                "usage": None,
        },
    ]


@pytest.mark.asyncio
async def test_codex_runtime_accepts_large_jsonl_records(tmp_path: Path, monkeypatch) -> None:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    fake_codex = bin_dir / "codex"
    large_text = "x" * 70_000
    fake_codex.write_text(
        "#!/usr/bin/env python3\n"
        "import json\n"
        f"print({json.dumps(json.dumps({'type': 'item.completed', 'item': {'type': 'agent_message', 'text': large_text}}))})\n"
        f"print({json.dumps(json.dumps({'type': 'turn.completed', 'session_id': 'session-1'}))})\n",
        encoding="utf-8",
    )
    fake_codex.chmod(0o755)
    monkeypatch.setenv("PATH", str(bin_dir) + os.pathsep + os.environ.get("PATH", ""))

    async def fake_metadata(cwd: str | None = None) -> dict[str, str]:
        return {}

    monkeypatch.setattr("agent_manager.providers.codex.fetch_codex_runtime_metadata", fake_metadata)

    runtime = CodexRuntime(AgentConfig(
        title="codex",
        provider="codex",
        cwd=str(tmp_path),
        permission_mode="workspace-write",
    ))

    await runtime.start()
    events = [event async for event in runtime.run_turn(AgentInput("hello"))]

    assert events[0] == {"type": "assistant_text", "text": large_text}
    assert events[1]["type"] == "result"
    assert events[1]["is_error"] is False


@pytest.mark.asyncio
async def test_codex_runtime_reports_stream_records_that_exceed_limit(tmp_path: Path, monkeypatch) -> None:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    fake_codex = bin_dir / "codex"
    large_text = "x" * 1_000
    fake_codex.write_text(
        "#!/usr/bin/env python3\n"
        "import json\n"
        f"print({json.dumps(json.dumps({'type': 'item.completed', 'item': {'type': 'agent_message', 'text': large_text}}))})\n",
        encoding="utf-8",
    )
    fake_codex.chmod(0o755)
    monkeypatch.setenv("PATH", str(bin_dir) + os.pathsep + os.environ.get("PATH", ""))
    monkeypatch.setattr("agent_manager.providers.codex.CODEX_STREAM_LIMIT", 128)

    async def fake_metadata(cwd: str | None = None) -> dict[str, str]:
        return {}

    monkeypatch.setattr("agent_manager.providers.codex.fetch_codex_runtime_metadata", fake_metadata)

    runtime = CodexRuntime(AgentConfig(
        title="codex",
        provider="codex",
        cwd=str(tmp_path),
        permission_mode="workspace-write",
    ))

    await runtime.start()
    events = [event async for event in runtime.run_turn(AgentInput("hello"))]

    assert events[0]["type"] == "error"
    assert events[0]["message"].startswith(
        "codex stdout emitted a single line larger than the 128 bytes stream limit"
    )
    assert "one tool result, diff, or assistant event was too large" in events[0]["message"]
    assert events[1] == {"type": "result", "subtype": "error", "is_error": True, "session_id": None}


@pytest.mark.asyncio
async def test_codex_runtime_resumes_after_capturing_session_id(tmp_path: Path, monkeypatch) -> None:
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    args_log = tmp_path / "args.log"
    fake_codex = bin_dir / "codex"
    fake_codex.write_text(
        "#!/bin/sh\n"
        "printf '%s\\n' \"$*\" >> \"$CODEX_ARGS_LOG\"\n"
        "printf '%s\\n' '{\"type\":\"thread.started\",\"id\":\"session-1\"}'\n"
        "printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}'\n"
        "printf '%s\\n' '{\"type\":\"turn.completed\",\"session_id\":\"session-1\"}'\n",
        encoding="utf-8",
    )
    fake_codex.chmod(0o755)
    monkeypatch.setenv("PATH", str(bin_dir) + os.pathsep + os.environ.get("PATH", ""))
    monkeypatch.setenv("CODEX_ARGS_LOG", str(args_log))

    async def fake_metadata(cwd: str | None = None) -> dict[str, str]:
        return {"configured_model": "gpt-default", "active_model_label": "gpt-default"}

    monkeypatch.setattr("agent_manager.providers.codex.fetch_codex_runtime_metadata", fake_metadata)

    runtime = CodexRuntime(AgentConfig(
        title="codex",
        provider="codex",
        cwd=str(tmp_path),
        permission_mode="workspace-write",
    ))

    await runtime.start()
    first = [event async for event in runtime.run_turn(AgentInput("first"))]
    second = [event async for event in runtime.run_turn(AgentInput("second"))]

    assert first[0]["data"]["session_id"] == "session-1"
    assert second[0]["data"]["resume"] is True
    assert second[0]["data"]["command"] == "codex exec resume"
    args = args_log.read_text(encoding="utf-8").splitlines()
    assert args[0].startswith(f"exec --json --cd {tmp_path} --sandbox workspace-write --skip-git-repo-check first")
    assert args[1].startswith("exec resume --json --skip-git-repo-check session-1 second")
    assert "agent-manager:artifact" in args[0]
    assert "agent-manager:artifact" in args[1]
