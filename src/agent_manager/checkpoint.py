"""Checkpoint files + handoff prompts.

Python port of the Go `sessionmgr/checkpoint.go`. Checkpoints live under the same
state directory the rest of the app uses (`AGENT_MANAGER_STATE_DIR`, default
`/var/lib/agent-manager`), namespaced per instance:

    {state_dir}/sessions/{title}/checkpoint-{N}.md

Freshness is tracked by modification time in **nanoseconds** (`st_mtime_ns`) so a
stale checkpoint left by a prior aborted attempt for the same session number can
never be mistaken for a fresh one (the respawn is gated on `mtime > baseline`).
"""

from __future__ import annotations

import os
import re
from pathlib import Path
from textwrap import dedent

from .persistence import DEFAULT_STATE_DIR

# Same constraint persistence.py enforces on titles, so a malformed title can't
# escape the sessions directory. App titles are slugified to [a-z0-9_-]+ already.
_TITLE_SAFE = re.compile(r"^[A-Za-z0-9_\-]+$")


def _sessions_root() -> Path:
    env_dir = os.environ.get("AGENT_MANAGER_STATE_DIR")
    return Path(env_dir or DEFAULT_STATE_DIR) / "sessions"


def session_dir(title: str) -> Path:
    if not _TITLE_SAFE.match(title):
        raise ValueError(f"unsafe instance title for filesystem: {title!r}")
    return _sessions_root() / title


def checkpoint_path(title: str, session_num: int) -> Path:
    return session_dir(title) / f"checkpoint-{session_num}.md"


def ensure_session_dir(title: str) -> None:
    session_dir(title).mkdir(parents=True, exist_ok=True)


def checkpoint_exists(title: str, session_num: int) -> bool:
    return checkpoint_path(title, session_num).is_file()


def checkpoint_mtime_ns(title: str, session_num: int) -> tuple[int, bool]:
    """Return (mtime_ns, exists). When the file is absent, returns (0, False) — so
    a zero baseline means "any newly written file qualifies as fresh"."""
    try:
        st = checkpoint_path(title, session_num).stat()
    except FileNotFoundError:
        return 0, False
    return st.st_mtime_ns, True


def read_checkpoint(title: str, session_num: int) -> str:
    return checkpoint_path(title, session_num).read_text(encoding="utf-8")


def cleanup_session_dir(title: str) -> None:
    """Remove all checkpoint files for an instance (used on instance delete)."""
    import shutil

    d = session_dir(title)
    if d.exists():
        shutil.rmtree(d)


# --- prompts (ported from checkpoint.go) ----------------------------------

_SECTIONS = dedent(
    """\
    The file must contain the following sections:

    ## CURRENT STATE
    What you were doing and what step you are on.

    ## COMPLETED
    What has been accomplished so far (list of steps/tasks completed).

    ## PENDING
    What remains to be done.

    ## CONTEXT
    Any decisions, constraints, or important observations that must carry forward.

    ## NEXT ACTION
    The exact next thing the continuation session should do.

    ## FILES
    Key file paths relevant to the current state (do NOT include file contents, only paths)."""
)


def wrap_up_prompt(title: str, session_num: int) -> str:
    path = checkpoint_path(title, session_num)
    return (
        f"You are approaching a session boundary. Before this session ends, write a checkpoint file to:\n"
        f"  {path}\n\n"
        f"{_SECTIONS}\n\n"
        f"Write the checkpoint file now, then confirm you are done.\n\n"
        f"IMPORTANT: After completing your current task, you MUST address the user's message above. "
        f"Do not ignore it."
    )


def hard_wrap_up_prompt(title: str, session_num: int) -> str:
    path = checkpoint_path(title, session_num)
    return (
        f"STOP. Your context window is nearly full and this session is ending NOW.\n\n"
        f"Drop whatever you are doing. Do NOT continue your current task. "
        f"Write a checkpoint file IMMEDIATELY to:\n"
        f"  {path}\n\n"
        f"{_SECTIONS}\n\n"
        f"Write this file NOW. This is your top priority — a new session will continue your work."
    )


def restore_prompt(title: str, session_num: int) -> str:
    path = checkpoint_path(title, session_num)
    return (
        f"You are continuing a workflow from a previous session. Read the checkpoint file at:\n"
        f"  {path}\n\n"
        f"This file describes what was accomplished, what remains, and what to do next.\n"
        f"Resume execution from where the checkpoint indicates. Do not re-do completed work."
    )
