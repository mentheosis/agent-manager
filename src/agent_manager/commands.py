from __future__ import annotations

import os
import sqlite3
import time
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .providers.base import AgentInput


@dataclass(frozen=True)
class AgentCommand:
    raw: str
    name: str
    args: list[str]


def parse_agent_command(message: AgentInput) -> AgentCommand | None:
    text = message.text.strip()
    if not text.startswith("/"):
        return None
    parts = text.split(maxsplit=1)
    if not parts:
        return None
    args = parts[1].split() if len(parts) > 1 else []
    return AgentCommand(raw=text, name=parts[0].lower(), args=args)


def handle_agent_command(
    command: AgentCommand,
    *,
    provider: str,
    session_id: str | None,
    has_images: bool = False,
) -> list[dict[str, Any]]:
    if has_images:
        return [_command_error(command.raw, "Slash commands cannot include image attachments.")]

    if provider != "codex":
        return [_command_error(command.raw, f"Slash commands are not supported for {provider} sessions.")]

    if command.name in {"/goal", "/plan"}:
        return [_handle_codex_goal_command(command.raw, command.args, session_id)]

    return [_command_error(command.raw, f"Unsupported Codex command: {command.raw}")]


def _handle_codex_goal_command(raw: str, args: list[str], session_id: str | None) -> dict[str, Any]:
    if not args:
        return _get_codex_goal(raw, session_id)

    action = args[0].lower()
    if len(args) == 1 and action == "clear":
        return _clear_codex_goal(raw, session_id)
    if len(args) == 1 and action in {"pause", "resume"}:
        return _set_codex_goal_status(raw, session_id, "paused" if action == "pause" else "active")

    return _set_codex_goal(raw, session_id, _goal_objective_from_raw(raw))


def _clear_codex_goal(raw: str, session_id: str | None) -> dict[str, Any]:
    if not session_id:
        return _command_error(raw, "Cannot clear goal before Codex has reported a session id.")

    db_path = _codex_goals_db()
    if not db_path.is_file():
        return _command_error(raw, f"Codex goals database was not found: {db_path}")

    try:
        with sqlite3.connect(db_path, timeout=5) as con:
            cur = con.execute("DELETE FROM thread_goals WHERE thread_id = ?", (session_id,))
            deleted = cur.rowcount
            con.commit()
    except sqlite3.Error as e:
        return _command_error(raw, f"Failed to clear Codex goal: {e}")

    if deleted:
        message = f"Cleared Codex goal for session {session_id}."
    else:
        message = f"No Codex goal was set for session {session_id}."
    return _command_result(raw, message, {"session_id": session_id, "deleted": deleted})


def _get_codex_goal(raw: str, session_id: str | None) -> dict[str, Any]:
    if not session_id:
        return _command_error(raw, "Cannot read goal before Codex has reported a session id.")

    db_path = _codex_goals_db()
    if not db_path.is_file():
        return _command_error(raw, f"Codex goals database was not found: {db_path}")

    try:
        with sqlite3.connect(db_path, timeout=5) as con:
            con.row_factory = sqlite3.Row
            row = con.execute(
                """
                SELECT
                    thread_id,
                    objective,
                    status,
                    token_budget,
                    tokens_used,
                    time_used_seconds,
                    created_at_ms,
                    updated_at_ms
                FROM thread_goals
                WHERE thread_id = ?
                """,
                (session_id,),
            ).fetchone()
    except sqlite3.Error as e:
        return _command_error(raw, f"Failed to read Codex goal: {e}")

    if row is None:
        return _command_result(raw, f"No Codex goal is set for session {session_id}.", {"session_id": session_id})

    goal = {
        "thread_id": row["thread_id"],
        "objective": row["objective"],
        "status": row["status"],
        "token_budget": row["token_budget"],
        "tokens_used": row["tokens_used"],
        "time_used_seconds": row["time_used_seconds"],
        "created_at_ms": row["created_at_ms"],
        "updated_at_ms": row["updated_at_ms"],
    }
    return _command_result(raw, _goal_summary(goal), {"session_id": session_id, "goal": goal})


def _set_codex_goal(raw: str, session_id: str | None, objective: str) -> dict[str, Any]:
    if not session_id:
        return _command_error(raw, "Cannot set goal before Codex has reported a session id.")
    if not objective:
        return _command_error(raw, "Usage: /goal <objective>")

    db_path = _codex_goals_db()
    if not db_path.is_file():
        return _command_error(raw, f"Codex goals database was not found: {db_path}")

    now_ms = int(time.time() * 1000)
    goal_id = str(uuid.uuid4())
    try:
        with sqlite3.connect(db_path, timeout=5) as con:
            con.execute(
                """
                INSERT INTO thread_goals (
                    thread_id,
                    goal_id,
                    objective,
                    status,
                    token_budget,
                    tokens_used,
                    time_used_seconds,
                    created_at_ms,
                    updated_at_ms
                ) VALUES (?, ?, ?, 'active', NULL, 0, 0, ?, ?)
                ON CONFLICT(thread_id) DO UPDATE SET
                    goal_id = excluded.goal_id,
                    objective = excluded.objective,
                    status = 'active',
                    token_budget = NULL,
                    tokens_used = 0,
                    time_used_seconds = 0,
                    updated_at_ms = excluded.updated_at_ms
                """,
                (session_id, goal_id, objective, now_ms, now_ms),
            )
            con.commit()
    except sqlite3.Error as e:
        return _command_error(raw, f"Failed to set Codex goal: {e}")

    return _command_result(
        raw,
        f"Set Codex goal for session {session_id}.",
        {"session_id": session_id, "objective": objective},
    )


def _set_codex_goal_status(raw: str, session_id: str | None, status: str) -> dict[str, Any]:
    if not session_id:
        return _command_error(raw, "Cannot update goal before Codex has reported a session id.")

    db_path = _codex_goals_db()
    if not db_path.is_file():
        return _command_error(raw, f"Codex goals database was not found: {db_path}")

    now_ms = int(time.time() * 1000)
    try:
        with sqlite3.connect(db_path, timeout=5) as con:
            cur = con.execute(
                """
                UPDATE thread_goals
                SET status = ?, updated_at_ms = ?
                WHERE thread_id = ?
                """,
                (status, now_ms, session_id),
            )
            updated = cur.rowcount
            con.commit()
    except sqlite3.Error as e:
        return _command_error(raw, f"Failed to update Codex goal: {e}")

    if updated:
        message = f"Set Codex goal status to {status} for session {session_id}."
    else:
        message = f"No Codex goal was set for session {session_id}."
    return _command_result(raw, message, {"session_id": session_id, "status": status, "updated": updated})


def _goal_objective_from_raw(raw: str) -> str:
    parts = raw.split(maxsplit=1)
    if len(parts) < 2:
        return ""
    return parts[1].strip()


def _goal_summary(goal: dict[str, Any]) -> str:
    lines = [
        f"Goal status: {goal['status']}",
        f"Objective: {goal['objective']}",
        f"Tokens used: {goal['tokens_used']}",
        f"Time used: {goal['time_used_seconds']}s",
    ]
    if goal.get("token_budget") is not None:
        lines.append(f"Token budget: {goal['token_budget']}")
    return "\n".join(lines)


def _codex_goals_db() -> Path:
    home = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    return home / "goals_1.sqlite"


def _command_result(raw: str, message: str, data: dict[str, Any] | None = None) -> dict[str, Any]:
    return {
        "type": "command_result",
        "command": raw,
        "message": message,
        "is_error": False,
        "data": data or {},
        "handled_at_ms": int(time.time() * 1000),
    }


def _command_error(raw: str, message: str) -> dict[str, Any]:
    return {
        "type": "command_result",
        "command": raw,
        "message": message,
        "is_error": True,
        "handled_at_ms": int(time.time() * 1000),
    }
