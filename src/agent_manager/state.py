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
        log.info("load_from_disk: found %d persisted instance(s)", len(records))
        if not records:
            return
        for rec in records:
            log.info("  -> loading instance %r (session_id=%s)", rec.title, rec.session_id)
            inst = Instance(
                title=rec.title,
                path=rec.path,
                permission_mode=rec.permission_mode,
                model=rec.model or None,
                display_title=rec.display_title,
                session_id=rec.session_id,
                created_at=rec.created_at,
                add_dirs=list(rec.add_dirs or []),
                instance_type=rec.instance_type or "claude",
                parent=rec.parent,
                children=list(rec.children or []),
                agent_preset=rec.agent_preset,
                task=rec.task,
            )
            inst._history = await self.persistence.load_events(rec.title)
            # Restore _next_seq so events published in this run never collide
            # with seq numbers already present in the persisted JSONL.
            # Old events written before seq was introduced have no "seq" field;
            # leaving _next_seq at 0 is safe for those — new events start at 0
            # and old clients use since_seq=-1 which skips the filter entirely.
            if inst._history:
                max_seq = max((e.get("seq", -1) for e in inst._history), default=-1)
                if max_seq >= 0:
                    inst._next_seq = max_seq + 1
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
        model: str | None = None,
        add_dirs: list[str] | None = None,
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
        resolved_dirs = _normalize_dirs(add_dirs or [])
        async with self._lock:
            title = self._unique_title_locked(base)
            display_title = cleaned if cleaned != title else None
            inst = Instance(
                title=title,
                path=expanded,
                permission_mode=permission_mode,
                model=model or None,
                display_title=display_title,
                created_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                add_dirs=resolved_dirs,
            )
            self._wire_hooks(inst)
            self._instances[title] = inst
        await inst.start()
        await self._save_records()
        return inst

    async def update_permissions(
        self,
        title: str,
        permission_mode: str | None = None,
        model: str | None = None,
        add_dirs: list[str] | None = None,
    ) -> Instance | None:
        """Update permission_mode / model / add_dirs and reload the SDK session."""
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            if permission_mode is not None:
                inst.permission_mode = permission_mode
            if model is not None:
                inst.model = model if model else None
            if add_dirs is not None:
                inst.add_dirs = _normalize_dirs(add_dirs)
        await self._save_records()
        await inst.reload_options()
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

    # --- Orchestration methods -----------------------------------------------

    async def reparent(self, title: str, new_parent: str | None) -> Instance | None:
        """Move an instance to a new parent (or remove from parent if None).

        Validation:
        - Loop instances cannot be reparented
        - Orchestrator-preset agents cannot be moved out of their team
        - New parent must be a loop instance (if not None)
        """
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None

            # Validation
            if inst.instance_type == "loop":
                raise ValueError("loop instances cannot be reparented")
            if inst.agent_preset == "orchestrator" and new_parent is None:
                raise ValueError("orchestrator agents cannot be removed from their team")

            if new_parent is not None:
                parent_inst = self._instances.get(new_parent)
                if parent_inst is None:
                    raise ValueError(f"parent instance not found: {new_parent}")
                if parent_inst.instance_type != "loop":
                    raise ValueError("can only reparent into loop instances")

            # Remove from old parent's children list
            old_parent = inst.parent
            if old_parent:
                old_parent_inst = self._instances.get(old_parent)
                if old_parent_inst and title in old_parent_inst.children:
                    old_parent_inst.children.remove(title)

            # Add to new parent's children list
            if new_parent:
                parent_inst = self._instances.get(new_parent)
                if parent_inst and title not in parent_inst.children:
                    parent_inst.children.append(title)

            inst.parent = new_parent

        await self._save_records()
        return inst

    def get_children(self, title: str) -> list[Instance]:
        """Get child instances of a loop instance."""
        inst = self._instances.get(title)
        if inst is None:
            return []
        return [self._instances[t] for t in inst.children if t in self._instances]

    async def update_task(self, title: str, task: str | None) -> Instance | None:
        """Update the task description for a loop instance."""
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            if inst.instance_type != "loop":
                raise ValueError("task can only be set on loop instances")
            inst.task = task
        await self._save_records()
        return inst

    async def update_instance_type(
        self,
        title: str,
        instance_type: str | None = None,
        agent_preset: str | None = None,
    ) -> Instance | None:
        """Update instance_type and/or agent_preset."""
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            if instance_type is not None:
                if instance_type not in ("claude", "loop"):
                    raise ValueError("instance_type must be 'claude' or 'loop'")
                inst.instance_type = instance_type
            if agent_preset is not None:
                if agent_preset not in ("coder", "researcher", "orchestrator", ""):
                    raise ValueError("agent_preset must be 'coder', 'researcher', 'orchestrator', or empty")
                inst.agent_preset = agent_preset if agent_preset else None
        await self._save_records()
        return inst

    async def _save_records(self) -> None:
        if self.persistence is None:
            return
        async with self._lock:
            records = [
                InstanceRecord(
                    title=i.title,
                    path=i.path,
                    permission_mode=i.permission_mode,
                    model=i.model or None,
                    display_title=i.display_title,
                    session_id=i.session_id,
                    created_at=i.created_at,
                    add_dirs=list(i.add_dirs or []),
                    instance_type=i.instance_type,
                    parent=i.parent,
                    children=list(i.children or []),
                    agent_preset=i.agent_preset,
                    task=i.task,
                )
                for i in self._instances.values()
            ]
        await self.persistence.save_instances(records)


def _normalize_dirs(dirs: list[str]) -> list[str]:
    """Resolve, dedupe, and drop empties — keep insertion order otherwise."""
    out: list[str] = []
    seen: set[str] = set()
    for d in dirs:
        if not d:
            continue
        try:
            p = str(Path(d).expanduser().resolve())
        except (OSError, RuntimeError):
            continue
        if p and p not in seen:
            seen.add(p)
            out.append(p)
    return out
