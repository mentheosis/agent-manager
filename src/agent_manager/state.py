from __future__ import annotations

import asyncio
import datetime as dt
import logging
import re
from pathlib import Path
from typing import Any

from .instance import Event, Instance
from .persistence import InstanceRecord, Persistence

log = logging.getLogger(__name__)


_UNSET = object()
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
    s = "".join(ch for ch in s if ch.isascii())
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
        # Per-provider AuthRegistry instances, injected by the server so we can
        # flag/clear the re-auth state based on runtime turn outcomes. Optional
        # so tests that build a bare Registry don't need to wire auth.
        self.auth_registries: dict[str, Any] | None = None

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
                provider=rec.provider or "claude",
                kind=rec.kind or ("loop" if rec.instance_type == "loop" else "agent"),
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
                folder=rec.folder,
                memory_file=rec.memory_file,
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
            self._handle_auth_signal(inst, event)
            await self.persistence.append_event(title, event)

        async def on_state_change() -> None:
            await self._save_records()

        inst._on_event = on_event
        inst._on_state_change = on_state_change

    def _handle_auth_signal(self, inst: Instance, event: Event) -> None:
        """Update the provider's AuthRegistry based on a turn's outcome.

        An ``auth_error`` event flags the provider for re-authentication; a
        successful ``result`` clears the flag (auth is demonstrably working
        again), so the UI self-heals without requiring a manual re-login.
        """
        if not self.auth_registries:
            return
        etype = event.get("type")
        registry = self.auth_registries.get(inst.provider)
        if registry is None:
            return
        if etype == "auth_error":
            registry.mark_needs_reauth(
                event.get("reason") or "expired",
                event.get("message"),
            )
        elif etype == "result" and not event.get("is_error") and registry.needs_reauth:
            registry.clear_needs_reauth()

    async def create(
        self,
        name: str,
        path: str,
        permission_mode: str = "acceptEdits",
        model: str | None = None,
        add_dirs: list[str] | None = None,
        provider: str = "claude",
        kind: str = "agent",
        memory_file: str | None = None,
    ) -> Instance:
        """Create an instance from a free-form display name.

        The canonical `title` is derived by slugifying the name and appending
        `_2`, `_3`, ... on collision. The original name is stored as
        `display_title` if it differs from the canonical title.
        """
        cleaned = name.strip()
        if not cleaned:
            raise ValueError("name must not be empty")
        provider = _normalize_provider(provider)
        kind = _normalize_kind(kind)
        permission_mode = _normalize_permission_mode(provider, permission_mode)
        base = slugify(cleaned)
        expanded = str(Path(path).expanduser().resolve())

        # Create directory if it doesn't exist
        expanded_path = Path(expanded)
        if not expanded_path.exists():
            log.info("Creating directory for instance: %s", expanded)
            expanded_path.mkdir(parents=True, exist_ok=True)

        resolved_dirs = _normalize_dirs(add_dirs or [])
        async with self._lock:
            title = self._unique_title_locked(base)
            display_title = cleaned if cleaned != title else None
            inst = Instance(
                title=title,
                path=expanded,
                provider=provider,
                kind=kind,
                instance_type="loop" if kind == "loop" else provider,
                permission_mode=permission_mode,
                model=model or None,
                display_title=display_title,
                created_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                add_dirs=resolved_dirs,
                memory_file=memory_file.strip() if memory_file else None,
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
        model: str | None | object = _UNSET,
        add_dirs: list[str] | None = None,
        memory_file: str | None | object = _UNSET,
        path: str | None = None,
    ) -> Instance | None:
        """Update permission_mode / model / add_dirs / memory_file / path and reload the SDK session."""
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            if permission_mode is not None:
                inst.permission_mode = _normalize_permission_mode(inst.provider, permission_mode)
            if model is not _UNSET:
                inst.model = model if isinstance(model, str) and model else None
            if add_dirs is not None:
                inst.add_dirs = _normalize_dirs(add_dirs)
            if memory_file is not _UNSET:
                inst.memory_file = memory_file if isinstance(memory_file, str) and memory_file else None
            if path is not None:
                expanded = str(Path(path).expanduser().resolve())
                expanded_path = Path(expanded)
                if not expanded_path.exists():
                    raise ValueError(f"path does not exist: {expanded}")
                inst.path = expanded
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

    async def delete(self, title: str, cascade: bool = True) -> bool:
        """Delete an instance.

        Args:
            title: The instance title to delete
            cascade: If True and this is a loop instance, also delete all children

        Returns:
            True if the instance was deleted, False if not found
        """
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return False

            # Collect child instances to delete if this is a loop instance
            child_instances: list[Instance] = []
            if cascade and inst.kind == "loop" and inst.children:
                child_instances = [
                    self._instances[c] for c in inst.children if c in self._instances
                ]

            # Remove the instance and children from registry
            self._instances.pop(title, None)
            for child in child_instances:
                self._instances.pop(child.title, None)

        # Stop the parent instance
        await inst.stop()
        if self.persistence is not None:
            await self.persistence.delete_events(title)

        # Stop and cleanup children
        for child in child_instances:
            log.info("Cascade deleting child instance: %s", child.title)
            await child.stop()
            if self.persistence is not None:
                await self.persistence.delete_events(child.title)

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
            if inst.kind == "loop":
                raise ValueError("loop instances cannot be reparented")
            if inst.agent_preset == "orchestrator" and new_parent is None:
                raise ValueError("orchestrator agents cannot be removed from their team")

            if new_parent is not None:
                parent_inst = self._instances.get(new_parent)
                if parent_inst is None:
                    raise ValueError(f"parent instance not found: {new_parent}")
                if parent_inst.kind != "loop":
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
            if inst.kind != "loop":
                raise ValueError("task can only be set on loop instances")
            inst.task = task
        await self._save_records()
        return inst

    async def update_instance_type(
        self,
        title: str,
        instance_type: str | None = None,
        kind: str | None = None,
        provider: str | None = None,
        agent_preset: str | None = None,
    ) -> Instance | None:
        """Update instance kind/provider compatibility fields and/or agent_preset."""
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            if kind is not None:
                inst.kind = _normalize_kind(kind)
            if provider is not None:
                normalized_provider = _normalize_provider(provider)
                inst.provider = normalized_provider
            if instance_type is not None:
                if instance_type == "loop":
                    inst.kind = "loop"
                elif instance_type in ("claude", "agent"):
                    inst.kind = "agent"
                    inst.provider = "claude"
                elif instance_type == "codex":
                    inst.kind = "agent"
                    inst.provider = "codex"
                else:
                    raise ValueError("instance_type must be 'claude', 'agent', 'codex', or 'loop'")
            inst.sync_instance_type()
            if agent_preset is not None:
                if agent_preset not in ("coder", "researcher", "orchestrator", ""):
                    raise ValueError("agent_preset must be 'coder', 'researcher', 'orchestrator', or empty")
                inst.agent_preset = agent_preset if agent_preset else None
        await self._save_records()
        return inst

    async def update_folder(self, title: str, folder: str | None) -> Instance | None:
        """Update the folder for an instance (for sidebar organization)."""
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            inst.folder = folder.strip() if folder else None
        await self._save_records()
        return inst

    async def update_memory_file(self, title: str, memory_file: str | None) -> Instance | None:
        """Update the memory file path for an instance.

        The contents of this file will be prepended to every prompt sent to the agent.
        """
        async with self._lock:
            inst = self._instances.get(title)
            if inst is None:
                return None
            inst.memory_file = memory_file.strip() if memory_file else None
        await self._save_records()
        return inst

    def get_folders(self) -> list[str]:
        """Get list of unique folder names across all instances."""
        folders: set[str] = set()
        for inst in self._instances.values():
            if inst.folder:
                folders.add(inst.folder)
        return sorted(folders)

    async def _save_records(self) -> None:
        if self.persistence is None:
            return
        async with self._lock:
            records = [
                InstanceRecord(
                    title=i.title,
                    path=i.path,
                    provider=i.provider,
                    kind=i.kind,
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
                    folder=i.folder,
                    memory_file=i.memory_file,
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


def _normalize_provider(provider: str | None) -> str:
    value = (provider or "claude").strip().lower()
    if value not in ("claude", "codex"):
        raise ValueError("provider must be 'claude' or 'codex'")
    return value


def _normalize_permission_mode(provider: str, permission_mode: str | None) -> str:
    mode = (permission_mode or "").strip()
    if provider == "codex" and mode in {"", "default", "acceptEdits", "plan", "bypassPermissions"}:
        return {
            "": "workspace-write",
            "default": "workspace-write",
            "acceptEdits": "workspace-write",
            "plan": "read-only",
            "bypassPermissions": "danger-full-access",
        }[mode]
    if provider == "codex" and mode not in {"workspace-write", "read-only", "danger-full-access"}:
        return "workspace-write"
    return mode or "acceptEdits"


def _normalize_kind(kind: str | None) -> str:
    value = (kind or "agent").strip().lower()
    if value not in ("agent", "loop"):
        raise ValueError("kind must be 'agent' or 'loop'")
    return value
