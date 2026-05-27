from __future__ import annotations

import sqlite3

from agent_manager.commands import handle_agent_command, parse_agent_command
from agent_manager.providers.base import AgentInput


def test_parse_slash_command_with_args() -> None:
    command = parse_agent_command(AgentInput("  /goal clear  "))

    assert command is not None
    assert command.name == "/goal"
    assert command.args == ["clear"]


def test_parse_goal_command_allows_multiline_prose_with_quotes() -> None:
    command = parse_agent_command(AgentInput("/goal Improve realism\nDo not copy the image's panel style."))

    assert command is not None
    assert command.name == "/goal"
    assert command.args == [
        "Improve",
        "realism",
        "Do",
        "not",
        "copy",
        "the",
        "image's",
        "panel",
        "style.",
    ]


def test_plain_prompt_is_not_command() -> None:
    assert parse_agent_command(AgentInput("please run /goal clear")) is None


def test_unsupported_slash_command_returns_error() -> None:
    command = parse_agent_command(AgentInput("/unknown"))  # type: ignore[arg-type]
    assert command is not None

    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events == [
        {
            "type": "command_result",
            "command": "/unknown",
            "message": "Unsupported Codex command: /unknown",
            "is_error": True,
            "handled_at_ms": events[0]["handled_at_ms"],
        }
    ]


def test_goal_clear_deletes_current_codex_thread_goal(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
        con.execute(
            """
            INSERT INTO thread_goals (
                thread_id, goal_id, objective, status, created_at_ms, updated_at_ms
            ) VALUES ('session-1', 'goal-1', 'Do work', 'active', 1, 1)
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    command = parse_agent_command(AgentInput("/goal clear"))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["type"] == "command_result"
    assert events[0]["is_error"] is False
    assert events[0]["data"] == {"session_id": "session-1", "deleted": 1}
    with sqlite3.connect(db) as con:
        rows = con.execute("SELECT * FROM thread_goals").fetchall()
    assert rows == []


def test_goal_text_sets_current_codex_thread_goal(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    command = parse_agent_command(AgentInput("/goal use Build the ship interior"))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["type"] == "command_result"
    assert events[0]["is_error"] is False
    assert events[0]["data"] == {
        "session_id": "session-1",
        "objective": "use Build the ship interior",
    }
    with sqlite3.connect(db) as con:
        rows = con.execute(
            "SELECT thread_id, objective, status, tokens_used, time_used_seconds FROM thread_goals"
        ).fetchall()
    assert rows == [("session-1", "use Build the ship interior", "active", 0, 0)]


def test_multiline_goal_text_sets_current_codex_thread_goal(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    text = "/goal Improve realism\nDo not copy the image's panel style."
    command = parse_agent_command(AgentInput(text))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["type"] == "command_result"
    assert events[0]["is_error"] is False
    assert events[0]["data"] == {
        "session_id": "session-1",
        "objective": "Improve realism\nDo not copy the image's panel style.",
    }
    with sqlite3.connect(db) as con:
        rows = con.execute("SELECT objective FROM thread_goals WHERE thread_id = 'session-1'").fetchall()
    assert rows == [("Improve realism\nDo not copy the image's panel style.",)]


def test_goal_without_args_reports_current_codex_thread_goal(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
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
            ) VALUES ('session-1', 'goal-1', 'Do work', 'paused', 1000, 25, 12, 1, 2)
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    command = parse_agent_command(AgentInput("/goal"))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["type"] == "command_result"
    assert events[0]["is_error"] is False
    assert events[0]["message"] == "\n".join(
        [
            "Goal status: paused",
            "Objective: Do work",
            "Tokens used: 25",
            "Time used: 12s",
            "Token budget: 1000",
        ]
    )
    assert events[0]["data"]["goal"] == {
        "thread_id": "session-1",
        "objective": "Do work",
        "status": "paused",
        "token_budget": 1000,
        "tokens_used": 25,
        "time_used_seconds": 12,
        "created_at_ms": 1,
        "updated_at_ms": 2,
    }


def test_goal_without_args_reports_missing_codex_thread_goal(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    command = parse_agent_command(AgentInput("/plan"))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["type"] == "command_result"
    assert events[0]["is_error"] is False
    assert events[0]["message"] == "No Codex goal is set for session session-1."
    assert events[0]["data"] == {"session_id": "session-1"}


def test_goal_pause_and_resume_update_current_codex_goal(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
        con.execute(
            """
            INSERT INTO thread_goals (
                thread_id, goal_id, objective, status, created_at_ms, updated_at_ms
            ) VALUES ('session-1', 'goal-1', 'Do work', 'active', 1, 1)
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    pause = parse_agent_command(AgentInput("/goal pause"))
    assert pause is not None
    pause_events = handle_agent_command(pause, provider="codex", session_id="session-1")
    assert pause_events[0]["data"] == {"session_id": "session-1", "status": "paused", "updated": 1}

    resume = parse_agent_command(AgentInput("/goal resume"))
    assert resume is not None
    resume_events = handle_agent_command(resume, provider="codex", session_id="session-1")
    assert resume_events[0]["data"] == {"session_id": "session-1", "status": "active", "updated": 1}

    with sqlite3.connect(db) as con:
        rows = con.execute("SELECT status FROM thread_goals WHERE thread_id = 'session-1'").fetchall()
    assert rows == [("active",)]


def test_plan_alias_uses_goal_management(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
        con.execute(
            """
            INSERT INTO thread_goals (
                thread_id, goal_id, objective, status, created_at_ms, updated_at_ms
            ) VALUES ('session-1', 'goal-1', 'Do work', 'active', 1, 1)
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    command = parse_agent_command(AgentInput("/plan pause"))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["is_error"] is False
    assert events[0]["data"] == {"session_id": "session-1", "status": "paused", "updated": 1}


def test_plan_alias_sets_goal_text_without_prefix_leak(tmp_path, monkeypatch) -> None:
    codex_home = tmp_path / ".codex"
    codex_home.mkdir()
    db = codex_home / "goals_1.sqlite"
    with sqlite3.connect(db) as con:
        con.execute(
            """
            CREATE TABLE thread_goals (
                thread_id TEXT PRIMARY KEY NOT NULL,
                goal_id TEXT NOT NULL,
                objective TEXT NOT NULL,
                status TEXT NOT NULL,
                token_budget INTEGER,
                tokens_used INTEGER NOT NULL DEFAULT 0,
                time_used_seconds INTEGER NOT NULL DEFAULT 0,
                created_at_ms INTEGER NOT NULL,
                updated_at_ms INTEGER NOT NULL
            )
            """
        )
    monkeypatch.setenv("CODEX_HOME", str(codex_home))

    command = parse_agent_command(AgentInput("/plan Build the ship interior"))
    assert command is not None
    events = handle_agent_command(command, provider="codex", session_id="session-1")

    assert events[0]["is_error"] is False
    assert events[0]["data"] == {
        "session_id": "session-1",
        "objective": "Build the ship interior",
    }
    with sqlite3.connect(db) as con:
        rows = con.execute("SELECT objective FROM thread_goals WHERE thread_id = 'session-1'").fetchall()
    assert rows == [("Build the ship interior",)]
