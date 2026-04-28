from __future__ import annotations

import asyncio
import json
import logging
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)

DEFAULT_STATE_DIR = Path("/var/lib/agent-manager")
HISTORY_REPLAY_CAP = 2000

# Titles already pass a frontend regex of ^[A-Za-z0-9_\-]+$ but enforce here too
# so a malformed title can't escape the events directory.
_TITLE_SAFE = re.compile(r"^[A-Za-z0-9_\-]+$")


@dataclass
class InstanceRecord:
    """Persisted instance metadata."""

    title: str
    path: str
    permission_mode: str = "acceptEdits"
    display_title: str | None = None
    session_id: str | None = None
    created_at: str = ""
    add_dirs: list[str] = None  # type: ignore[assignment]

    def __post_init__(self) -> None:
        if self.add_dirs is None:
            self.add_dirs = []

    def to_dict(self) -> dict[str, Any]:
        return {
            "title": self.title,
            "path": self.path,
            "permission_mode": self.permission_mode,
            "display_title": self.display_title,
            "session_id": self.session_id,
            "created_at": self.created_at,
            "add_dirs": list(self.add_dirs or []),
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "InstanceRecord":
        return cls(
            title=d["title"],
            path=d["path"],
            permission_mode=d.get("permission_mode") or "acceptEdits",
            display_title=d.get("display_title"),
            session_id=d.get("session_id"),
            created_at=d.get("created_at") or "",
            add_dirs=list(d.get("add_dirs") or []),
        )


class Persistence:
    """File-backed persistence for instance metadata and event logs.

    Layout under state_dir:
      instances.json           — full ordered registry, atomically rewritten on change
      events/{title}.jsonl     — append-only event log, one JSON object per line
    """

    def __init__(self, state_dir: str | Path | None = None) -> None:
        env_dir = os.environ.get("AGENT_MANAGER_STATE_DIR")
        self.state_dir = Path(state_dir or env_dir or DEFAULT_STATE_DIR)
        self.events_dir = self.state_dir / "events"
        self.instances_file = self.state_dir / "instances.json"
        self._instances_lock = asyncio.Lock()
        self._event_locks: dict[str, asyncio.Lock] = {}

    def ensure_dirs(self) -> None:
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self.events_dir.mkdir(parents=True, exist_ok=True)

    def _event_file(self, title: str) -> Path:
        if not _TITLE_SAFE.match(title):
            raise ValueError(f"unsafe title for filesystem: {title!r}")
        return self.events_dir / f"{title}.jsonl"

    def _event_lock(self, title: str) -> asyncio.Lock:
        lock = self._event_locks.get(title)
        if lock is None:
            lock = asyncio.Lock()
            self._event_locks[title] = lock
        return lock

    # --- instances.json ---------------------------------------------------

    async def load_instances(self) -> list[InstanceRecord]:
        async with self._instances_lock:
            if not self.instances_file.exists():
                return []
            try:
                raw = self.instances_file.read_text(encoding="utf-8")
                data = json.loads(raw) if raw.strip() else []
            except (OSError, json.JSONDecodeError) as e:
                log.exception("failed to load instances: %s", e)
                return []
        if not isinstance(data, list):
            log.warning("instances.json root is not a list; ignoring")
            return []
        out: list[InstanceRecord] = []
        for d in data:
            if not isinstance(d, dict):
                continue
            try:
                out.append(InstanceRecord.from_dict(d))
            except Exception as e:
                log.warning("skipping malformed instance record: %s", e)
        return out

    async def save_instances(self, records: list[InstanceRecord]) -> None:
        async with self._instances_lock:
            self.state_dir.mkdir(parents=True, exist_ok=True)
            tmp = self.instances_file.with_suffix(".json.tmp")
            tmp.write_text(
                json.dumps([r.to_dict() for r in records], indent=2),
                encoding="utf-8",
            )
            tmp.replace(self.instances_file)

    # --- events ------------------------------------------------------------

    async def append_event(self, title: str, event: dict[str, Any]) -> None:
        try:
            path = self._event_file(title)
        except ValueError:
            return
        lock = self._event_lock(title)
        async with lock:
            self.events_dir.mkdir(parents=True, exist_ok=True)
            line = json.dumps(event, ensure_ascii=False)
            with path.open("a", encoding="utf-8") as f:
                f.write(line + "\n")

    async def load_events(self, title: str, cap: int = HISTORY_REPLAY_CAP) -> list[dict[str, Any]]:
        try:
            path = self._event_file(title)
        except ValueError:
            return []
        if not path.exists():
            return []
        lock = self._event_lock(title)
        async with lock:
            try:
                lines = path.read_text(encoding="utf-8").splitlines()
            except OSError as e:
                log.warning("failed to read events for %s: %s", title, e)
                return []
        events: list[dict[str, Any]] = []
        for line in lines:
            line = line.strip()
            if not line:
                continue
            try:
                events.append(json.loads(line))
            except json.JSONDecodeError:
                continue
        if cap and len(events) > cap:
            events = events[-cap:]
        return events

    async def delete_events(self, title: str) -> None:
        try:
            path = self._event_file(title)
        except ValueError:
            return
        lock = self._event_lock(title)
        async with lock:
            if path.exists():
                try:
                    path.unlink()
                except OSError as e:
                    log.warning("failed to delete events for %s: %s", title, e)
        self._event_locks.pop(title, None)
