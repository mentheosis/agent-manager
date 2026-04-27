from __future__ import annotations

import asyncio
import datetime as dt
import logging
import re
from pathlib import Path

from .instance import Event, Instance
from .persistence import InstanceRecord, Persistence

log = logging.getLogger(__name__)


_SLUG_WS = re.compile(r"\s+")
_SLUG_BAD = re.compile(r"[^a-z0-9_-]")
_SLUG_DUP_UNDERSCORE = re.compile(r"_+")


def slugify(name: str, max_len: int = 64) -> str:
    """Reduce a free-form name to a canonical id matching [a-z0-9_-]+.

    Whitespace becomes underscore; everything else outside [a-z0-9_-] is
    dropped. Returns "instance" if the result would be empty.
    """
    s = name.lower().strip()
    s = _SLUG_WS.sub("_", s)
    s = _SLUG_BAD.sub("", s)
    s = _SLUG_DUP_UNDERSCORE.sub("_", s)
    s = s.strip("_-")
    if not s:
        return "instance"
    return s[:max_len]


class Registry:
    """Instance registry backed by file persistence.

    Every change to instance metadata (create/delete/rename/reorder, plus
    session_id capture from inside the Instance) triggers a full rewrite of
    instances.json. Every published event is appended to its instance's
    events/{title}.jsonl. On startup, load_from_disk() rehydrates the registry
    and recreates Instance background tasks with their stored session_ids so
    the SDK resumes the prior conversation.
    """

    def __init__(self, persistence: Persistence | None = None) -> None:
        self._instances: dict[str, Instance] = {}
        self._lock = asyncio.Lock()
        self.persistence = persistence

    async def load_from_disk(self) -> None:
        """Read persisted state, rebuild Instance objects, start their tasks."""
        if self.persistence is None:
            return
        records = await self.persistence.load_instances()
        if not records:
            return
        for rec in records:
            inst = Instance(
                title=rec.title,
                path=rec.path,
                permission_mode=rec.permission_mode,
                display_title=rec.display_title,
                session_id=rec.session_id,
                created_at=rec.created_at,
            )
            inst._history = await self.persistence.load_events(rec.title)
            self._wire_hooks(inst)
            async with self._lock:
                self._instances[rec.title] = inst
        # Start tasks outside the lock to avoid contention.
        for inst in list(self._instances.values()):
            await inst.start()
        log.info("loaded %d instance(s) from disk", len(self._instances))

    def _wire_hooks(self, inst: Instance) -> None:
        if self.persistence is None:
            return
        title = inst.title

        async def on_event(event: Event) -> None:
            await self.persistence.append_event(title, event)

        async def on_state_change() -> None:
            await self._save_records()

        inst._on_event = on_event
        inst._on_state_change = on_state_change

    async def create(
        self,
        name: str,
        path: str,
        permission_mode: str = "acceptEdits",
    ) -> Instance:
        """Create an instance from a free-form display name.

        The canonical `title` is derived by slugifying the name and appending
        `_2`, `_3`, ... on collision. The original name is stored as
        `display_title` if it differs from the canonical title.
        """
        cleaned = name.strip()
        if not cleaned:
            raise ValueError("name must not be empty")
        base = slugify(cleaned)
        expanded = str(Path(path).expanduser().resolve())
        async with self._lock:
            title = self._unique_title_locked(base)
            display_title = cleaned if cleaned != title else None
            inst = Instance(
                title=title,
                path=expanded,
                permission_mode=permission_mode,
                display_title=display_title,
                created_at=dt.datetime.now(dt.timezone.utc).isoformat(),
            )
            self._wire_hooks(inst)
            self._instances[title] = inst
        await inst.start()
        await self._save_records()
        return inst

    def _unique_title_locked(self, base: str) -> str:
        """Return base or base_N for the first N>=2 that's free. Caller holds lock."""
        if base not in self._instances:
            return base
        for i in range(2, 10_000):
            candidate = f"{base}_{i}"
            if candidate not in self._instances:
                return candidate
        raise ValueError(f"too many existing instances with base name {base!r}")

    def get(self, title: str) -> Instance | None:
        return self._instances.get(title)

    def list(self) -> list[Instance]:
        return list(self._instances.values())

    async def delete(self, title: str) -> bool:
        async with self._lock:
            inst = self._instances.pop(title, None)
        if inst is None:
            return False
        await inst.stop()
        if self.persistence is not None:
            await self.persistence.delete_events(title)
        await self._save_records()
        return True

    async def rename(self, title: str, display_title: str | None) -> Instance | None:
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            cleaned = display_title.strip() if display_title else None
            inst.display_title = cleaned or None
        await self._save_records()
        return inst

    async def reorder(self, ordered_titles: list[str]) -> None:
        async with self._lock:
            existing = set(self._instances.keys())
            requested = set(ordered_titles)
            if requested != existing:
                missing = existing - requested
                extra = requested - existing
                raise ValueError(
                    f"reorder titles must match exactly; missing={sorted(missing)} extra={sorted(extra)}"
                )
            self._instances = {t: self._instances[t] for t in ordered_titles}
        await self._save_records()

    async def shutdown(self) -> None:
        async with self._lock:
            instances = list(self._instances.values())
            self._instances.clear()
        for inst in instances:
            await inst.stop()

    async def _save_records(self) -> None:
        if self.persistence is None:
            return
        async with self._lock:
            records = [
                InstanceRecord(
                    title=i.title,
                    path=i.path,
                    permission_mode=i.permission_mode,
                    display_title=i.display_title,
                    session_id=i.session_id,
                    created_at=i.created_at,
                )
                for i in self._instances.values()
            ]
        await self.persistence.save_instances(records)
