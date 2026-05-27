from __future__ import annotations

import json
import re
from typing import Any

from agent_manager.artifacts import artifact_path_is_present, image_artifact_event, is_image_path

from .base import AgentEvent

_PRICE_PER_M_TOKENS: dict[str, tuple[float, float, float]] = {
    "gpt-5.2-codex": (1.75, 0.175, 14.00),
    "gpt-5.1-codex-max": (1.25, 0.125, 10.00),
    "gpt-5.1-codex": (1.25, 0.125, 10.00),
    "gpt-5-codex": (1.25, 0.125, 10.00),
    "gpt-5.5": (5.00, 0.50, 30.00),
    "gpt-5.4-mini": (0.75, 0.075, 4.50),
    "gpt-5.4": (2.50, 0.25, 15.00),
    "gpt-5.2": (1.75, 0.175, 14.00),
    "gpt-5.1": (1.25, 0.125, 10.00),
    "gpt-5-mini": (0.25, 0.025, 2.00),
    "gpt-5-nano": (0.05, 0.005, 0.40),
    "gpt-5": (1.25, 0.125, 10.00),
}


def translate_codex_event(raw: dict[str, Any], system_context: dict[str, Any] | None = None) -> list[AgentEvent]:
    """Translate one Codex JSONL event into agent-manager's normalized events.

    Codex event payloads have changed across CLI releases. Keep this parser
    permissive: prefer documented top-level event names and inspect common
    nested item shapes without making the UI depend on provider-specific JSON.
    """
    event_type = str(raw.get("type") or "")
    events: list[AgentEvent] = []

    if event_type == "thread.started":
        sid = _first_str(raw, "session_id", "thread_id", "id", "conversation_id")
        thread = _dict(raw.get("thread"))
        sid = sid or _first_str(thread, "session_id", "thread_id", "id")
        data: dict[str, Any] = dict(system_context or {})
        data["provider"] = "codex"
        if sid:
            data["session_id"] = sid
        model = _first_str(raw, "model") or _first_str(thread, "model")
        if model:
            data["model"] = model
        events.append({"type": "system_init", "data": data})
        return events

    if event_type in ("turn.completed", "turn.failed"):
        usage = raw.get("usage")
        usage_dict = usage if isinstance(usage, dict) else None
        model = _first_str(raw, "model") or _first_str(system_context or {}, "model", "active_model_label", "configured_model", "requested_model")
        estimated_cost = _estimate_cost_usd(model, usage_dict)
        events.append(
            {
                "type": "result",
                "subtype": "success" if event_type == "turn.completed" else "error",
                "duration_ms": raw.get("duration_ms"),
                "num_turns": raw.get("num_turns"),
                "total_cost_usd": raw.get("total_cost_usd"),
                "estimated_cost_usd": estimated_cost,
                "estimated_cost_model": model if estimated_cost is not None else None,
                "is_error": event_type == "turn.failed",
                "session_id": _first_str(raw, "session_id", "thread_id", "id"),
                "usage": usage_dict,
            }
        )
        if event_type == "turn.failed":
            message = _text_from(raw) or "Codex turn failed"
            events.insert(0, {"type": "error", "message": message})
        return events

    if event_type == "error":
        message = _text_from(raw) or "Codex error"
        return [{"type": "error", "message": message}]

    if event_type.startswith("item."):
        item = _dict(raw.get("item")) or _dict(raw.get("data")) or raw
        item_type = str(
            raw.get("item_type")
            or item.get("type")
            or item.get("kind")
            or item.get("item_type")
            or ""
        )
        item_id = str(raw.get("item_id") or item.get("id") or item.get("call_id") or "")

        if _is_reasoning_item(item_type):
            text = _text_from(item) or _text_from(raw)
            if text:
                events.append({"type": "thinking", "text": text})
            return events

        if _is_assistant_message(item_type, item):
            text = _text_from(item) or _text_from(raw)
            if text:
                events.append({"type": "assistant_text", "text": text})
            return events

        if _is_tool_item(item_type):
            tool_name = str(item.get("name") or item.get("tool_name") or item_type or "codex_tool")
            tool_id = item_id or f"codex-{abs(hash(str(raw))) & 0xffffffff:x}"
            if event_type == "item.started":
                events.append(_tool_use_event(tool_id, tool_name, _tool_input_from(item, raw)))
            elif event_type in {"item.failed", "item.cancelled"}:
                output = _output_from(item) or _output_from(raw) or item_type
                events.append(
                    {
                        "type": "tool_result",
                        "tool_id": tool_id,
                        "output": output,
                        "is_error": True,
                    }
                )
            else:
                output = _output_from(item) or _output_from(raw)
                events.append(
                    {
                        "type": "tool_result",
                        "tool_id": tool_id,
                        "output": output,
                        "is_error": _is_error_item(item, output),
                    }
                )
            if _is_image_tool_item(item_type, tool_name):
                events.extend(_image_artifact_events_from_raw(raw))
            return events

        text = _text_from(item) or _text_from(raw)
        if text:
            events.append({"type": "assistant_text", "text": text})

    return events


def translate_codex_transcript_event(raw: dict[str, Any], system_context: dict[str, Any] | None = None) -> list[AgentEvent]:
    """Translate high-value records from Codex's persisted session transcript.

    The `codex exec --json` stdout stream is intentionally compact and can
    collapse file edits into anonymous `file_change` items. The persisted
    transcript contains richer patch records. Only emit events that add detail
    we do not reliably get from stdout, so the UI does not double-render every
    shell command and assistant message.
    """
    payload = _dict(raw.get("payload"))
    if not payload:
        return []

    payload_type = str(payload.get("type") or "")
    if raw.get("type") == "response_item" and payload_type in {
        "function_call",
        "custom_tool_call",
        "tool_search_call",
        "web_search_call",
        "image_generation_call",
    }:
        name = str(payload.get("name") or "custom_tool_call")
        tool_id = _first_str(payload, "call_id", "id") or f"codex-patch-{abs(hash(str(raw))) & 0xffffffff:x}"
        return [
            _tool_use_event(
                tool_id,
                name if name != "custom_tool_call" else payload_type,
                payload.get("input") or _tool_input_from(payload, raw),
            )
        ]

    if raw.get("type") == "response_item" and payload_type in {
        "function_call_output",
        "tool_search_output",
    }:
        tool_id = _first_str(payload, "call_id", "id") or f"codex-result-{abs(hash(str(raw))) & 0xffffffff:x}"
        output = _output_from(payload) or _json_compact(payload)
        return [
            {
                "type": "tool_result",
                "tool_id": tool_id,
                "output": output,
                "is_error": _is_error_item(payload, output),
            }
        ]

    if raw.get("type") == "event_msg" and payload_type == "patch_apply_end":
        tool_id = _first_str(payload, "call_id", "id") or f"codex-patch-{abs(hash(str(raw))) & 0xffffffff:x}"
        return [
            {
                "type": "tool_result",
                "tool_id": tool_id,
                "output": _patch_apply_output(payload),
                "is_error": not bool(payload.get("success", payload.get("status") == "completed")),
            }
        ]

    if raw.get("type") == "event_msg" and payload_type == "mcp_tool_call_end":
        return _mcp_tool_call_events(payload)

    if raw.get("type") == "event_msg" and payload_type == "agent_message":
        message = payload.get("message")
        if isinstance(message, str) and message.strip():
            return [{"type": "assistant_text", "text": message}]
        return []

    if raw.get("type") == "event_msg" and payload_type == "task_complete":
        return [
            {
                "type": "result",
                "subtype": "success",
                "duration_ms": payload.get("duration_ms"),
                "is_error": False,
                "terminal": True,
            }
        ]

    if _is_image_tool_item(payload_type, str(payload.get("name") or "")):
        return _image_artifact_events_from_raw(raw)
    return []


def _dict(value: Any) -> dict[str, Any]:
    return value if isinstance(value, dict) else {}


def _first_str(data: dict[str, Any], *keys: str) -> str | None:
    for key in keys:
        value = data.get(key)
        if isinstance(value, str) and value:
            return value
    return None


def _text_from(data: dict[str, Any]) -> str:
    for key in ("text", "delta", "message", "summary", "content"):
        value = data.get(key)
        if isinstance(value, str):
            return value
        if isinstance(value, list):
            parts: list[str] = []
            for part in value:
                if isinstance(part, str):
                    parts.append(part)
                elif isinstance(part, dict):
                    text = part.get("text") or part.get("content")
                    if isinstance(text, str):
                        parts.append(text)
            if parts:
                return "".join(parts)
    error = data.get("error")
    if isinstance(error, str):
        return error
    if isinstance(error, dict):
        message = error.get("message")
        if isinstance(message, str):
            return message
    return ""


def _output_from(data: dict[str, Any]) -> str:
    for key in ("output", "aggregated_output", "stdout", "stderr", "result"):
        value = data.get(key)
        if isinstance(value, str):
            return value
        if value is not None:
            return str(value)
    return _text_from(data)


def _patch_apply_output(data: dict[str, Any]) -> str:
    parts: list[str] = []
    stdout = data.get("stdout")
    stderr = data.get("stderr")
    if isinstance(stdout, str) and stdout.strip():
        parts.append(stdout.strip())
    if isinstance(stderr, str) and stderr.strip():
        parts.append(stderr.strip())
    changes = data.get("changes")
    if isinstance(changes, dict) and changes:
        summary = []
        for path, change in changes.items():
            kind = "update"
            if isinstance(change, dict):
                kind = str(change.get("type") or kind)
            summary.append(f"{kind}: {path}")
        parts.append("Changed files:\n" + "\n".join(summary))
    return "\n\n".join(parts) or _json_compact(data)


def _mcp_tool_call_events(payload: dict[str, Any]) -> list[AgentEvent]:
    invocation = _dict(payload.get("invocation"))
    tool_id = _first_str(payload, "call_id", "id") or f"codex-mcp-{abs(hash(str(payload))) & 0xffffffff:x}"
    server = invocation.get("server")
    tool = invocation.get("tool")
    tool_name = "mcp_tool_call"
    if isinstance(tool, str) and tool:
        tool_name = f"mcp.{tool}"

    result = _dict(payload.get("result"))
    output = _mcp_result_output(result)
    return [
        _tool_use_event(
            tool_id,
            tool_name,
            {
                "server": server,
                "tool": tool,
                "arguments": invocation.get("arguments") or {},
            },
        ),
        {
            "type": "tool_result",
            "tool_id": tool_id,
            "output": output,
            "is_error": _mcp_result_is_error(result),
        },
    ]


def _tool_use_event(tool_id: str, name: str, input_value: Any) -> AgentEvent:
    event: AgentEvent = {
        "type": "tool_use",
        "id": tool_id,
        "name": name,
        "input": input_value,
    }
    if _tool_concludes_turn(name, input_value):
        event["concludes_turn"] = True
    display_text = _tool_display_text(name, input_value)
    if display_text:
        event["display_text"] = display_text
    return event


def _tool_display_text(name: str, input_value: Any) -> str:
    if name == "update_goal":
        goal_input = _coerce_json_object(input_value)
        status = goal_input.get("status")
        if status == "complete":
            return "Goal marked complete."
        if status == "blocked":
            return "Goal marked blocked."
        return ""

    if name != "update_plan":
        return ""

    plan_input = _coerce_json_object(input_value)
    if not plan_input:
        return ""

    lines: list[str] = []
    explanation = plan_input.get("explanation")
    if isinstance(explanation, str) and explanation.strip():
        lines.append(explanation.strip())

    in_progress_steps = _in_progress_plan_steps(plan_input.get("plan"))
    if in_progress_steps:
        lines.extend(f"In progress: {step}" for step in in_progress_steps)

    return "\n".join(lines) or "Plan updated."


def _tool_concludes_turn(name: str, input_value: Any) -> bool:
    if name != "update_goal":
        return False
    goal_input = _coerce_json_object(input_value)
    return goal_input.get("status") in {"complete", "blocked"}


def _coerce_json_object(value: Any) -> dict[str, Any]:
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError:
            return {}
        return parsed if isinstance(parsed, dict) else {}
    return {}


def _in_progress_plan_steps(plan: Any) -> list[str]:
    if not isinstance(plan, list):
        return []

    steps: list[str] = []
    for item in plan:
        if not isinstance(item, dict):
            continue
        status = item.get("status")
        if status not in {"in_progress", "in_progess"}:
            continue
        step = item.get("step")
        if isinstance(step, str) and step.strip():
            steps.append(step.strip())
    return steps


def _mcp_result_output(result: dict[str, Any]) -> str:
    if "Err" in result:
        return _json_compact(result["Err"])
    ok = _dict(result.get("Ok"))
    content = ok.get("content")
    if isinstance(content, list):
        parts: list[str] = []
        for item in content:
            if isinstance(item, str):
                parts.append(item)
            elif isinstance(item, dict):
                text = item.get("text") or item.get("content")
                if isinstance(text, str):
                    parts.append(text)
                else:
                    parts.append(_json_compact(item))
            else:
                parts.append(_json_compact(item))
        if parts:
            return "\n".join(parts)
    return _json_compact(result)


def _mcp_result_is_error(result: dict[str, Any]) -> bool:
    if "Err" in result:
        return True
    ok = _dict(result.get("Ok"))
    return bool(ok.get("isError") or ok.get("is_error"))


def _json_compact(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    try:
        return json.dumps(value, ensure_ascii=False)
    except TypeError:
        return str(value)


def _tool_input_from(item: dict[str, Any], raw: dict[str, Any]) -> Any:
    for data in (item, raw):
        for key in ("input", "command", "arguments"):
            value = data.get(key)
            if value not in (None, "", {}):
                return value

    details: dict[str, Any] = {}
    for key in (
        "query",
        "search_query",
        "url",
        "urls",
        "path",
        "pattern",
        "cmd",
        "action",
    ):
        value = item.get(key, raw.get(key))
        if value not in (None, "", {}, []):
            details[key] = value
    if details:
        return details

    metadata_keys = {
        "id",
        "call_id",
        "type",
        "kind",
        "item_type",
        "name",
        "tool_name",
        "output",
        "aggregated_output",
        "stdout",
        "stderr",
        "result",
        "error",
        "content",
        "text",
        "summary",
    }
    return {
        key: value
        for key, value in item.items()
        if key not in metadata_keys and value not in (None, "", {}, [])
    }


def _is_reasoning_item(item_type: str) -> bool:
    return "reasoning" in item_type or item_type in {"thought", "thinking"}


def _is_assistant_message(item_type: str, item: dict[str, Any]) -> bool:
    if item.get("role") == "assistant":
        return True
    return item_type in {"agent_message", "assistant_message", "message", "assistant"}


def _is_tool_item(item_type: str) -> bool:
    return any(
        marker in item_type
        for marker in (
            "command",
            "tool",
            "mcp",
            "web_search",
            "file_change",
            "patch",
            "function_call",
            "custom_tool_call",
            "image_gen",
            "image_generation",
        )
    )


def _is_image_tool_item(item_type: str, tool_name: str) -> bool:
    combined = f"{item_type} {tool_name}".lower()
    return "image_gen" in combined or "image_generation" in combined


_IMAGE_PATH_RE = re.compile(r"(?P<path>(?:/|~\/)[^\s'\"<>]+?\.(?:png|jpg|jpeg|webp|gif))", re.IGNORECASE)


def _image_artifact_events_from_raw(raw: Any) -> list[AgentEvent]:
    paths: list[str] = []
    _collect_image_paths(raw, paths)
    seen: set[str] = set()
    events: list[AgentEvent] = []
    for path in paths:
        if path in seen:
            continue
        if not artifact_path_is_present(path):
            continue
        seen.add(path)
        events.append(image_artifact_event(path, source="codex"))
    return events


def _collect_image_paths(value: Any, paths: list[str]) -> None:
    if isinstance(value, dict):
        for key, item in value.items():
            if key in {"path", "file", "filename", "image", "image_path", "output", "result", "text", "content"}:
                _collect_image_paths(item, paths)
            elif isinstance(item, (dict, list)):
                _collect_image_paths(item, paths)
        return
    if isinstance(value, list):
        for item in value:
            _collect_image_paths(item, paths)
        return
    if not isinstance(value, str):
        return
    for match in _IMAGE_PATH_RE.finditer(value):
        path = match.group("path").rstrip(".,;:)")
        if is_image_path(path):
            paths.append(path)


def _looks_like_failed_output(output: str) -> bool:
    return "Process exited with code 1" in output or "Process exited with code 2" in output


def _is_error_item(item: dict[str, Any], output: str) -> bool:
    status = item.get("status")
    exit_code = item.get("exit_code")
    return bool(
        item.get("is_error")
        or item.get("error")
        or status in {"failed", "error", "cancelled"}
        or (isinstance(exit_code, int) and exit_code != 0)
        or _looks_like_failed_output(output)
    )


def _estimate_cost_usd(model: str | None, usage: dict[str, Any] | None) -> float | None:
    if not model or not usage:
        return None
    rates = _rates_for_model(model)
    if rates is None:
        return None

    input_rate, cached_input_rate, output_rate = rates
    input_tokens = _usage_int(usage, "input_tokens")
    cached_tokens = (
        _usage_int(usage, "cached_input_tokens")
        or _usage_int(usage, "cache_read_input_tokens")
        or _usage_int(usage, "cache_read")
    )
    output_tokens = _usage_int(usage, "output_tokens") + _usage_int(usage, "reasoning_output_tokens")
    uncached_input_tokens = max(input_tokens - cached_tokens, 0)
    cost = (
        uncached_input_tokens * input_rate
        + cached_tokens * cached_input_rate
        + output_tokens * output_rate
    ) / 1_000_000
    return cost


def _rates_for_model(model: str) -> tuple[float, float, float] | None:
    normalized = model.strip().lower()
    for key in sorted(_PRICE_PER_M_TOKENS, key=len, reverse=True):
        if normalized == key or normalized.startswith(f"{key}-"):
            return _PRICE_PER_M_TOKENS[key]
    return None


def _usage_int(usage: dict[str, Any], key: str) -> int:
    value = usage.get(key)
    return value if isinstance(value, int) else 0
