"""File-system helpers for the diff/settings/plans/memory tabs.

These tabs each surface a small directory of files for the agent's working
directory (or, for memory, the Claude CLI's per-project memory dir). The
helpers here concern path encoding, safe path validation, and reading a
directory of `.md` files into a JSON-serialisable list.
"""
from __future__ import annotations

import asyncio
import logging
import os
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)


def encode_project_path(path: str) -> str:
    """Encode a working-directory path the way Claude CLI does for ~/.claude/projects/.

    Strips the leading "/", replaces "/" and "_" with "-", and prefixes "-".
    e.g. "/Users/kris-w/wrk/foo" → "-Users-kris-w-wrk-foo".
    """
    s = path.lstrip("/")
    s = s.replace("/", "-").replace("_", "-")
    return f"-{s}"


def memory_dir_for(instance_path: str, home: Path | None = None) -> Path:
    """Return ~/.claude/projects/<encoded>/memory/ for the given instance path."""
    base = home if home is not None else Path.home()
    return base / ".claude" / "projects" / encode_project_path(instance_path) / "memory"


def safe_path_under(base: Path, requested: str) -> Path:
    """Resolve `requested` and assert it lives under `base`.

    Raises ValueError on escape attempts. Returned path is absolute & resolved.
    """
    abs_path = Path(requested).expanduser().resolve()
    base_resolved = base.resolve()
    try:
        abs_path.relative_to(base_resolved)
    except ValueError as e:
        raise ValueError(
            f"path {abs_path} is not under {base_resolved}"
        ) from e
    return abs_path


def read_md_files(directory: Path) -> list[dict[str, Any]]:
    """Read every `*.md` file (non-recursive) in `directory` as `{name, path, content}`."""
    out: list[dict[str, Any]] = []
    if not directory.is_dir():
        return out
    try:
        entries = sorted(directory.iterdir())
    except OSError as e:
        log.warning("failed to list %s: %s", directory, e)
        return out
    for entry in entries:
        if not entry.is_file() or entry.suffix != ".md":
            continue
        try:
            content = entry.read_text(encoding="utf-8", errors="replace")
        except OSError as e:
            log.warning("failed to read %s: %s", entry, e)
            continue
        out.append({"name": entry.name, "path": str(entry), "content": content})
    return out


def read_named_files(specs: list[tuple[str, Path]]) -> list[dict[str, Any]]:
    """Read each `(display_name, absolute_path)` into `{name, path, content, exists, writable}`.

    Always returns one entry per spec, even when the file doesn't exist.
    """
    out: list[dict[str, Any]] = []
    for name, path in specs:
        entry: dict[str, Any] = {
            "name": name,
            "path": str(path),
            "content": "",
            "exists": False,
            "writable": True,
        }
        try:
            entry["content"] = path.read_text(encoding="utf-8")
            entry["exists"] = True
        except FileNotFoundError:
            pass
        except OSError as e:
            log.warning("failed to read %s: %s", path, e)
        out.append(entry)
    return out


def write_file_under(base: Path, requested_path: str, content: str) -> Path:
    """Validate path, mkdir parent if needed, write content. Returns the absolute path."""
    target = safe_path_under(base, requested_path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(content, encoding="utf-8")
    return target


async def run_git(workdir: Path, *args: str) -> tuple[int, str, str]:
    """Run `git <args>` in workdir, return (returncode, stdout, stderr)."""
    if not workdir.is_dir():
        return (1, "", f"workdir does not exist: {workdir}")
    try:
        proc = await asyncio.create_subprocess_exec(
            "git", *args,
            cwd=str(workdir),
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await proc.communicate()
    except FileNotFoundError:
        return (1, "", "git binary not found")
    return (
        proc.returncode if proc.returncode is not None else 1,
        stdout.decode("utf-8", errors="replace"),
        stderr.decode("utf-8", errors="replace"),
    )
