from __future__ import annotations

import json
import logging
import mimetypes
import os
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import httpx

from fastapi import FastAPI, HTTPException, Request, WebSocket, WebSocketDisconnect
from fastapi.responses import FileResponse, Response
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field, model_validator

from .auth import AuthRegistry, CODEX_AUTH_PATH, CODEX_LOGIN_COMMAND, codex_auth_raw_status
from .artifacts import path_for_artifact_id
from .files import (
    memory_dir_for,
    read_md_files,
    read_named_files,
    run_git,
    write_file_under,
)
from .instance import Instance
from .orchestrator import get_manager as get_orchestrator_manager
from .persistence import Persistence
from .providers.capabilities import list_provider_capabilities, provider_capabilities
from .providers.codex_metadata import fetch_codex_models
from .state import Registry, _UNSET

log = logging.getLogger(__name__)

# Claude Code config paths
_CLAUDE_CONFIG = Path.home() / ".claude.json"
_CLAUDE_BACKUPS_DIR = Path.home() / ".claude" / "backups"


def _restore_claude_config_if_missing() -> None:
    """Restore .claude.json from backup if it doesn't exist.

    The Docker volume persists ~/.claude/ (including backups) but not ~/.claude.json.
    On container restart, restore from the latest backup to avoid CLI warnings.
    """
    if _CLAUDE_CONFIG.exists():
        return

    if not _CLAUDE_BACKUPS_DIR.is_dir():
        # No backups dir — create minimal config
        log.info("creating minimal %s (no backups found)", _CLAUDE_CONFIG)
        _CLAUDE_CONFIG.write_text("{}\n", encoding="utf-8")
        return

    # Find latest backup by modification time
    backups = list(_CLAUDE_BACKUPS_DIR.glob(".claude.json.backup.*"))
    if not backups:
        log.info("creating minimal %s (backup dir empty)", _CLAUDE_CONFIG)
        _CLAUDE_CONFIG.write_text("{}\n", encoding="utf-8")
        return

    latest = max(backups, key=lambda p: p.stat().st_mtime)
    try:
        content = latest.read_text(encoding="utf-8")
        _CLAUDE_CONFIG.write_text(content, encoding="utf-8")
        log.info("restored %s from %s", _CLAUDE_CONFIG, latest.name)
    except OSError as e:
        log.warning("failed to restore claude config: %s", e)
        _CLAUDE_CONFIG.write_text("{}\n", encoding="utf-8")


def _find_static_dir() -> Path | None:
    env = os.environ.get("AGENT_MANAGER_STATIC_DIR")
    if env:
        p = Path(env)
        return p if p.is_dir() else None
    here = Path(__file__).resolve().parent
    candidates = [
        here / "static",                  # packaged alongside the module
        here.parent.parent.parent / "static",  # source tree: repo/static next to src/
        Path("/app/static"),              # docker default
        Path.cwd() / "static",            # CWD fallback
    ]
    for p in candidates:
        if p.is_dir():
            return p
    return None


_ARTIFACT_PREVIEW_LIMIT_BYTES = 50 * 1024 * 1024
_FORBIDDEN_ARTIFACT_NAMES = {
    ".env",
    ".npmrc",
    ".pypirc",
    "id_rsa",
    "id_ed25519",
    "credentials",
    "credentials.json",
}


def _artifact_roots(extra_roots: list[str | Path] | None = None) -> list[Path]:
    roots = [
        Path("/app/.codex/generated_images"),
        Path.home() / ".codex" / "generated_images",
        Path("/tmp"),
        Path("/var/lib/agent-manager"),
    ]
    roots.extend(Path(root) for root in (extra_roots or []))
    return [root.resolve() for root in roots if root.exists()]


def _resolve_artifact(artifact_id: str, *, roots: list[Path]) -> Path:
    try:
        path = path_for_artifact_id(artifact_id).expanduser().resolve()
    except Exception as e:
        raise HTTPException(status_code=400, detail="invalid artifact id") from e
    if not path.is_file():
        raise HTTPException(status_code=404, detail="artifact not found")
    if not any(path == root or root in path.parents for root in roots):
        raise HTTPException(status_code=403, detail="artifact path is not allowed")
    if _is_forbidden_artifact_path(path):
        raise HTTPException(status_code=403, detail="artifact path is not allowed")
    try:
        if path.stat().st_size > _ARTIFACT_PREVIEW_LIMIT_BYTES:
            raise HTTPException(status_code=413, detail="artifact is too large to preview")
    except OSError as e:
        raise HTTPException(status_code=404, detail="artifact not found") from e
    return path


def _artifact_file_response(path: Path, media_type: str) -> FileResponse:
    return FileResponse(
        path,
        media_type=media_type,
        filename=_versioned_artifact_filename(path),
        content_disposition_type="inline",
        headers={
            "Cache-Control": "no-store, max-age=0",
            "Pragma": "no-cache",
            "Expires": "0",
        },
    )


def _versioned_artifact_filename(path: Path) -> str:
    stamp = int(time.time() * 1000)
    if path.suffix:
        return f"{path.stem}-{stamp}{path.suffix}"
    return f"{path.name}-{stamp}"


def _is_forbidden_artifact_path(path: Path) -> bool:
    lowered_parts = {part.lower() for part in path.parts}
    if lowered_parts.intersection({".ssh", ".aws", ".config"}):
        return True
    name = path.name.lower()
    return name in _FORBIDDEN_ARTIFACT_NAMES or "secret" in name or "token" in name


def _merge_settings_json(workdir: Path, settings: dict[str, Any]) -> None:
    """Merge settings into .claude/settings.json, creating it if needed.

    For `permissions.allow` and `permissions.deny`, arrays are merged (new
    items added to existing). Other top-level keys are shallow-merged.
    """
    settings_dir = workdir / ".claude"
    settings_file = settings_dir / "settings.json"

    settings_dir.mkdir(parents=True, exist_ok=True)

    existing: dict[str, Any] = {}
    if settings_file.exists():
        try:
            existing = json.loads(settings_file.read_text(encoding="utf-8"))
        except (json.JSONDecodeError, OSError):
            existing = {}

    # Special handling for permissions - merge arrays
    if "permissions" in settings:
        new_perms = settings.pop("permissions")
        existing_perms = existing.setdefault("permissions", {})
        for key in ("allow", "deny"):
            if key in new_perms:
                existing_list = existing_perms.get(key, [])
                # Add new items, avoiding duplicates
                for item in new_perms[key]:
                    if item not in existing_list:
                        existing_list.append(item)
                existing_perms[key] = existing_list

    # Shallow merge remaining keys
    merged = {**existing, **settings}
    settings_file.write_text(json.dumps(merged, indent=2) + "\n", encoding="utf-8")
    log.info("merged settings into %s", settings_file)


class CreateInstanceBody(BaseModel):
    name: str = Field(min_length=1)
    path: str = Field(min_length=1)
    provider: str = "claude"
    kind: str = "agent"
    permission_mode: str = "acceptEdits"
    model: str | None = None
    add_dirs: list[str] = Field(default_factory=list)
    settings_json: dict[str, Any] | None = None  # Settings to merge into .claude/settings.json


class ImageData(BaseModel):
    media_type: str  # e.g., "image/png", "image/jpeg"
    data: str  # base64-encoded image data


class SendBody(BaseModel):
    text: str = ""
    images: list[ImageData] | None = None

    @model_validator(mode="after")
    def require_text_or_images(self) -> "SendBody":
        if not self.text.strip() and not self.images:
            raise ValueError("Either text or images must be provided")
        return self


class InputBody(BaseModel):
    data: str = Field(min_length=1)


class RenameBody(BaseModel):
    display_title: str | None = None


class ReorderBody(BaseModel):
    titles: list[str]


class FileWriteBody(BaseModel):
    path: str = Field(min_length=1)
    content: str = ""


class PermissionsBody(BaseModel):
    permission_mode: str | None = None
    model: str | None = None
    add_dirs: list[str] | None = None
    memory_file: str | None = None
    path: str | None = None  # Working directory - can fix typos


class ReparentBody(BaseModel):
    parent: str | None = None  # New parent title, or None to remove from parent


class TaskBody(BaseModel):
    task: str | None = None


class InstanceTypeBody(BaseModel):
    instance_type: str | None = None  # Compatibility: "claude" | "agent" | "loop"
    kind: str | None = None  # "agent" | "loop"
    provider: str | None = None  # "claude" | "codex"
    agent_preset: str | None = None  # "coder" | "researcher" | "orchestrator"


class FolderBody(BaseModel):
    folder: str | None = None  # Folder name, or None to remove from folder


class MemoryFileBody(BaseModel):
    memory_file: str | None = None  # Path to memory file, or None to clear


class SessionConfigBody(BaseModel):
    # All optional — only the fields present in the request are merged. A 0 on a
    # numeric field means "use the default" (see SessionConfig.effective_*).
    enabled: bool | None = None
    soft_context_percentage: int | None = None
    hard_context_percentage: int | None = None
    checkpoint_timeout_sec: int | None = None
    context_window_size: int | None = None
    split_cooldown_sec: int | None = None


def _summary(inst: Instance) -> dict[str, Any]:
    return {
        "title": inst.title,
        "display_title": inst.display_title,
        "path": inst.path,
        "provider": inst.provider,
        "kind": inst.kind,
        "permission_mode": inst.permission_mode,
        "model": inst.model or None,
        "status": inst.status,
        "created_at": inst.created_at,
        "add_dirs": list(inst.add_dirs or []),
        # Orchestration fields
        "instance_type": inst.instance_type,
        "parent": inst.parent,
        "children": list(inst.children or []),
        "agent_preset": inst.agent_preset,
        "task": inst.task,
        # Organization
        "folder": inst.folder,
        # Memory
        "memory_file": inst.memory_file,
    }


def _find_instance_by_identifier(registry: Registry, identifier: str) -> Instance | None:
    inst = registry.get(identifier)
    if inst is not None:
        return inst
    for candidate in registry.list():
        if candidate.session_id == identifier:
            return candidate
    return None


def _codex_session_debug(session_id: str | None, raw_tail: int = 0) -> dict[str, Any] | None:
    if not session_id:
        return None
    sessions_dir = Path.home() / ".codex" / "sessions"
    matches = sorted(sessions_dir.glob(f"**/*{session_id}*.jsonl")) if sessions_dir.exists() else []
    if not matches:
        return {
            "session_id": session_id,
            "found": False,
            "sessions_dir": str(sessions_dir),
        }

    path = matches[-1]
    stat = path.stat()
    debug: dict[str, Any] = {
        "session_id": session_id,
        "found": True,
        "path": str(path),
        "size_bytes": stat.st_size,
        "mtime_unix": stat.st_mtime,
        "line_count": _line_count(path),
    }
    raw_tail = max(0, min(raw_tail, 20))
    if raw_tail:
        debug["raw_tail"] = _tail_text_lines(path, raw_tail, max_chars=120_000)
    return debug


def _line_count(path: Path) -> int:
    try:
        with path.open("rb") as f:
            return sum(1 for _ in f)
    except OSError:
        return 0


def _tail_text_lines(path: Path, count: int, max_chars: int = 80_000) -> list[str]:
    try:
        lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError:
        return []
    out = lines[-count:]
    total = 0
    trimmed: list[str] = []
    for line in reversed(out):
        total += len(line)
        if total > max_chars:
            break
        trimmed.append(line)
    return list(reversed(trimmed))


_models_cache: list[str] | None = None
_MODEL_PROVIDERS = ("claude", "codex")
_FALLBACK_MODELS = [
    "claude-opus-4-8",
    "claude-opus-4-7",
    "claude-sonnet-4-7",
    "claude-opus-4-5",
    "claude-sonnet-4-5",
    "claude-haiku-3-5",
]


async def _fetch_models() -> list[str]:
    """Fetch available models from the Anthropic API, caching after the first call."""
    global _models_cache
    if _models_cache is not None:
        return _models_cache
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    if not api_key:
        log.info("ANTHROPIC_API_KEY not set; using fallback Claude model list")
        return _FALLBACK_MODELS
    try:
        async with httpx.AsyncClient(timeout=10) as client:
            r = await client.get(
                "https://api.anthropic.com/v1/models",
                headers={"x-api-key": api_key, "anthropic-version": "2023-06-01"},
            )
            r.raise_for_status()
            data = r.json()
            ids = [m["id"] for m in data.get("data", []) if m.get("type") == "model"]
            if ids:
                _models_cache = ids
                log.info("fetched %d model(s) from Anthropic API", len(ids))
                return ids
    except Exception:
        log.warning("failed to fetch models from Anthropic API; using fallback Claude model list", exc_info=True)
    return _FALLBACK_MODELS


async def _fetch_provider_models(provider: str) -> list[str]:
    if provider == "claude":
        return await _fetch_models()
    if provider == "codex":
        return await fetch_codex_models()
    raise HTTPException(status_code=404, detail=f"unknown provider: {provider}")


async def _prefetch_provider_models() -> None:
    for provider in _MODEL_PROVIDERS:
        try:
            await _fetch_provider_models(provider)
        except Exception:
            log.exception("model pre-fetch failed for provider %s; will retry on first request", provider)


def build_app() -> FastAPI:
    persistence = Persistence()
    persistence.ensure_dirs()
    registry = Registry(persistence)
    auth = AuthRegistry()
    provider_auth = {
        "claude": auth,
        "codex": AuthRegistry(
            provider="codex",
            login_command=CODEX_LOGIN_COMMAND,
            credentials_path=CODEX_AUTH_PATH,
            status_func=codex_auth_raw_status,
            login_supported=True,
        ),
    }

    # Initialize orchestrator manager
    orchestrator_manager = get_orchestrator_manager(base_url="http://localhost:8765")

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        # Restore Claude CLI config from backup if missing (Docker volume edge case)
        _restore_claude_config_if_missing()
        try:
            await registry.load_from_disk()
        except Exception:
            log.exception("failed to load persisted state; continuing with empty registry")
        await _prefetch_provider_models()
        try:
            yield
        finally:
            await orchestrator_manager.shutdown()
            await registry.shutdown()
            for auth_registry in provider_auth.values():
                await auth_registry.shutdown()

    app = FastAPI(title="agent-manager", version="0.1.0", lifespan=lifespan)
    app.state.registry = registry
    app.state.auth = auth
    app.state.provider_auth = provider_auth
    app.state.persistence = persistence

    # Polling endpoints that should log at DEBUG instead of INFO
    _QUIET_PATHS = {
        "/api/auth/status",
        "/api/providers/claude/auth/status",
        "/api/providers/codex/auth/status",
        "/api/instances",
    }

    @app.middleware("http")
    async def access_log_middleware(request: Request, call_next):
        start = time.perf_counter()
        response = await call_next(request)
        duration_ms = (time.perf_counter() - start) * 1000
        path = request.url.path
        level = logging.DEBUG if (request.method == "GET" and path in _QUIET_PATHS) else logging.INFO
        log.log(level, '%s %s %s %.0fms', request.method, path, response.status_code, duration_ms)
        return response

    def _provider_auth(provider: str) -> AuthRegistry:
        auth_registry = provider_auth.get(provider)
        if auth_registry is None:
            raise HTTPException(status_code=404, detail=f"unknown provider: {provider}")
        return auth_registry

    @app.get("/api/providers")
    async def list_providers() -> list[dict[str, Any]]:
        return list_provider_capabilities()

    @app.get("/api/providers/{provider}")
    async def get_provider(provider: str) -> dict[str, Any]:
        try:
            return provider_capabilities(provider)
        except KeyError:
            raise HTTPException(status_code=404, detail=f"unknown provider: {provider}")

    @app.get("/api/providers/{provider}/models")
    async def list_provider_models(provider: str) -> list[str]:
        return await _fetch_provider_models(provider)

    @app.get("/api/models")
    async def list_models(provider: str = "claude") -> list[str]:
        return await _fetch_provider_models(provider)

    @app.get("/api/instances")
    async def list_instances() -> list[dict[str, Any]]:
        return [_summary(i) for i in registry.list()]

    @app.get("/api/debug/sessions/{identifier}")
    async def debug_session(identifier: str, tail: int = 20, raw_tail: int = 0) -> dict[str, Any]:
        """Inspect a running or persisted instance by title or provider session id."""
        inst = _find_instance_by_identifier(registry, identifier)
        if not inst:
            raise HTTPException(status_code=404)

        tail = max(1, min(tail, 100))
        history = inst.history()
        recent_events = history[-tail:] if tail < len(history) else history
        return {
            "matched_by": "title" if inst.title == identifier else "session_id",
            "instance": _summary(inst),
            "session_id": inst.session_id,
            "debug": inst.debug_state(),
            "recent_events": recent_events,
            "codex_session": _codex_session_debug(inst.session_id, raw_tail=raw_tail)
            if inst.provider == "codex"
            else None,
        }

    @app.get("/api/batch/scan")
    async def scan_batch_directory(directory: str) -> dict[str, Any]:
        """Scan a directory for YAML files and return parsed configs."""
        import yaml

        expanded = Path(directory).expanduser().resolve()
        if not expanded.is_dir():
            raise HTTPException(status_code=400, detail=f"Not a directory: {directory}")

        files: list[dict[str, Any]] = []
        for ext in ("*.yaml", "*.yml"):
            for filepath in expanded.glob(ext):
                entry: dict[str, Any] = {"filename": filepath.name, "path": str(filepath)}
                try:
                    content = filepath.read_text(encoding="utf-8")
                    config = yaml.safe_load(content)
                    if not isinstance(config, dict):
                        entry["error"] = "Invalid YAML structure (expected object)"
                    elif not config.get("name"):
                        entry["error"] = "Missing required field: name"
                    else:
                        entry["config"] = config
                except yaml.YAMLError as e:
                    entry["error"] = f"YAML parse error: {e}"
                except OSError as e:
                    entry["error"] = f"Read error: {e}"
                files.append(entry)

        # Sort by filename
        files.sort(key=lambda f: f["filename"])
        return {"directory": str(expanded), "files": files}

    @app.post("/api/instances", status_code=201)
    async def create_instance(body: CreateInstanceBody) -> dict[str, Any]:
        try:
            inst = await registry.create(
                body.name,
                body.path,
                body.permission_mode,
                body.model,
                body.add_dirs,
                provider=body.provider,
                kind=body.kind,
            )
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        except FileNotFoundError as e:
            raise HTTPException(status_code=400, detail=f"path not found: {e}")

        # Merge settings_json into .claude/settings.json if provided.
        # This payload is Claude-specific until provider_options replaces it.
        if body.settings_json and inst.provider == "claude":
            try:
                _merge_settings_json(Path(inst.path), body.settings_json)
            except Exception as e:
                log.warning("failed to merge settings.json for %s: %s", inst.title, e)

        return _summary(inst)

    @app.get("/api/instances/{title}")
    async def get_instance(title: str) -> dict[str, Any]:
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        return {**_summary(inst), "history": inst.history()}

    @app.delete("/api/instances/{title}", status_code=204)
    async def delete_instance(title: str) -> Response:
        # Stop orchestrator if this is a loop instance
        inst = registry.get(title)
        if inst and inst.kind == "loop":
            await orchestrator_manager.stop(title)

        ok = await registry.delete(title)
        if not ok:
            raise HTTPException(status_code=404)
        return Response(status_code=204)

    @app.patch("/api/instances/{title}/rename")
    async def rename_instance(title: str, body: RenameBody) -> dict[str, Any]:
        inst = await registry.rename(title, body.display_title)
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    @app.patch("/api/instances/{title}/permissions")
    async def update_permissions(title: str, body: PermissionsBody) -> dict[str, Any]:
        inst = await registry.update_permissions(
            title,
            permission_mode=body.permission_mode,
            model=body.model if "model" in body.model_fields_set else _UNSET,
            add_dirs=body.add_dirs,
            memory_file=body.memory_file if "memory_file" in body.model_fields_set else _UNSET,
            path=body.path,
        )
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    # --- Session management endpoints ----------------------------------------

    @app.get("/api/instances/{title}/session")
    async def get_session(title: str) -> dict[str, Any]:
        info = registry.session_info(title)
        if info is None:
            raise HTTPException(status_code=404)
        return info

    @app.put("/api/instances/{title}/session/config")
    async def put_session_config(title: str, body: SessionConfigBody) -> dict[str, Any]:
        inst = await registry.update_session_config(title, body.model_dump(exclude_unset=True))
        if inst is None:
            raise HTTPException(status_code=404)
        return registry.session_info(title)

    @app.post("/api/instances/{title}/session/split")
    async def split_session(title: str) -> dict[str, str]:
        """Arm a manual split; it fires when the agent is next idle."""
        res = registry.request_manual_split(title)
        if res is None:
            raise HTTPException(status_code=404)
        if not res:
            raise HTTPException(status_code=400, detail="session management is not enabled for this instance")
        return {"status": "manual_split_armed"}

    # --- Orchestration endpoints ---------------------------------------------

    @app.post("/api/instances/{title}/reparent")
    async def reparent_instance(title: str, body: ReparentBody) -> dict[str, Any]:
        try:
            inst = await registry.reparent(title, body.parent)
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    @app.get("/api/instances/{title}/children")
    async def get_children(title: str) -> list[dict[str, Any]]:
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        return [_summary(c) for c in registry.get_children(title)]

    @app.patch("/api/instances/{title}/task")
    async def update_task(title: str, body: TaskBody) -> dict[str, Any]:
        try:
            inst = await registry.update_task(title, body.task)
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    @app.patch("/api/instances/{title}/type")
    async def update_instance_type(title: str, body: InstanceTypeBody) -> dict[str, Any]:
        try:
            inst = await registry.update_instance_type(
                title,
                instance_type=body.instance_type,
                kind=body.kind,
                provider=body.provider,
                agent_preset=body.agent_preset,
            )
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    @app.patch("/api/instances/{title}/folder")
    async def update_folder(title: str, body: FolderBody) -> dict[str, Any]:
        """Update the folder for an instance (sidebar organization)."""
        inst = await registry.update_folder(title, body.folder)
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    @app.patch("/api/instances/{title}/memory-file")
    async def update_memory_file(title: str, body: MemoryFileBody) -> dict[str, Any]:
        """Update the memory file for an instance.

        The contents of this file will be prepended to every prompt sent to the agent.
        """
        inst = await registry.update_memory_file(title, body.memory_file)
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    @app.get("/api/folders")
    async def list_folders() -> list[str]:
        """Get list of all folder names."""
        return registry.get_folders()

    # --- Orchestrator process management --------------------------------------

    @app.post("/api/instances/{title}/orchestrator/start")
    async def start_orchestrator(title: str) -> dict[str, Any]:
        """Start the orchestrator process for a loop instance."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        if inst.kind != "loop":
            raise HTTPException(status_code=400, detail="can only start orchestrator for loop instances")

        children = registry.get_children(title)
        try:
            proc = await orchestrator_manager.start(inst, children)
            return {
                "ok": True,
                "pid": proc.pid,
                "port": proc.port,
                "running": proc.is_running,
            }
        except RuntimeError as e:
            raise HTTPException(status_code=500, detail=str(e))

    @app.post("/api/instances/{title}/orchestrator/stop")
    async def stop_orchestrator(title: str) -> dict[str, Any]:
        """Stop the orchestrator process for a loop instance."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)

        await orchestrator_manager.stop(title)
        return {"ok": True}

    @app.post("/api/instances/{title}/orchestrator/restart")
    async def restart_orchestrator(title: str) -> dict[str, Any]:
        """Restart the orchestrator process for a loop instance."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        if inst.kind != "loop":
            raise HTTPException(status_code=400, detail="can only restart orchestrator for loop instances")

        children = registry.get_children(title)
        try:
            proc = await orchestrator_manager.restart(inst, children)
            return {
                "ok": True,
                "pid": proc.pid,
                "port": proc.port,
                "running": proc.is_running,
            }
        except RuntimeError as e:
            raise HTTPException(status_code=500, detail=str(e))

    @app.get("/api/instances/{title}/orchestrator/status")
    async def get_orchestrator_status(title: str) -> dict[str, Any]:
        """Get the status of the orchestrator process for a loop instance."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)

        proc = orchestrator_manager.get(title)
        if not proc:
            return {"running": False, "pid": None, "port": None}

        return {
            "running": proc.is_running,
            "pid": proc.pid,
            "port": proc.port,
        }

    @app.get("/api/instances/{title}/orchestrator/output")
    async def get_orchestrator_output(title: str, lines: int = 50) -> dict[str, Any]:
        """Get recent output from the orchestrator process."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)

        lines = max(1, min(lines, 500))
        output = orchestrator_manager.get_output(title, lines)
        return {"lines": output}

    @app.get("/api/instances/{title}/history")
    async def get_history(
        title: str,
        tail: int = 50,
        offset: int = 0,
        types: str | None = None,
    ) -> dict[str, Any]:
        """Get instance history with backwards pagination.

        Args:
            tail: Number of events to return from the end (default: 50, max: 200)
            offset: Skip N events from end before taking tail (for scrolling back)
            types: Comma-separated event types to filter (e.g., "assistant_text,tool_result")

        Returns:
            {events: [...], total_count: N, has_more: bool}
        """
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)

        # Clamp tail to reasonable bounds
        tail = max(1, min(tail, 200))

        all_events = inst.history()
        total_count = len(all_events)

        # Filter by types if specified
        if types:
            type_set = {t.strip() for t in types.split(",") if t.strip()}
            all_events = [e for e in all_events if e.get("type") in type_set]

        filtered_count = len(all_events)

        # Apply offset and tail (reading backwards from end)
        if offset > 0:
            all_events = all_events[:-offset] if offset < len(all_events) else []

        events = all_events[-tail:] if tail < len(all_events) else all_events
        has_more = len(all_events) > tail

        return {
            "events": events,
            "total_count": total_count,
            "filtered_count": filtered_count,
            "has_more": has_more,
        }

    @app.get("/api/instances/{title}/artifacts/{artifact_id}")
    async def get_instance_artifact(title: str, artifact_id: str) -> FileResponse:
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        roots = _artifact_roots([inst.path, *list(inst.add_dirs or [])])
        path = _resolve_artifact(artifact_id, roots=roots)
        media_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        return _artifact_file_response(path, media_type)

    @app.get("/api/artifacts/images/{artifact_id}")
    async def get_image_artifact(artifact_id: str) -> FileResponse:
        path = _resolve_artifact(artifact_id, roots=_artifact_roots())
        media_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
        return _artifact_file_response(path, media_type)

    @app.post("/api/instances/reorder")
    async def reorder_instances(body: ReorderBody) -> list[dict[str, Any]]:
        try:
            await registry.reorder(body.titles)
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        return [_summary(i) for i in registry.list()]

    @app.post("/api/instances/{title}/send")
    async def send(title: str, body: SendBody) -> dict[str, Any]:
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        images = None
        if body.images:
            images = [{"media_type": img.media_type, "data": img.data} for img in body.images]
        await inst.send(body.text, images=images)
        return {"ok": True}

    @app.post("/api/instances/{title}/abort")
    async def abort_instance(title: str) -> dict[str, Any]:
        """Abort the current operation and restart the SDK client."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        await inst.abort()
        return {"ok": True}

    @app.websocket("/api/instances/{title}/events")
    async def events_ws(ws: WebSocket, title: str, since_seq: int = -1) -> None:
        inst = registry.get(title)
        if not inst:
            await ws.close(code=1008, reason="instance not found")
            return
        await ws.accept()
        # since_seq lets reconnecting clients receive only events they missed.
        # -1 (default) means "send everything" — used on initial connection.
        # Any value >= 0 is a reconnect: only send events with seq > since_seq.
        # Because seq is a monotonic counter on the instance (never an array index),
        # this is correct even when the history window has been trimmed by HISTORY_CAP.
        # Using seq < 0 as the "send all" sentinel also means old in-memory events
        # without a seq field are always included on initial connection.
        for event in inst.history():
            if since_seq < 0 or event.get("seq", -1) > since_seq:
                await ws.send_text(json.dumps(event))
        # Signal to the client that all buffered history has been sent.
        # The client uses this to batch-render history and scroll once rather
        # than scrolling incrementally as each history frame arrives.
        await ws.send_text(json.dumps({"type": "history_end"}))
        q = inst.subscribe()
        try:
            while True:
                event = await q.get()
                await ws.send_text(json.dumps(event))
        except WebSocketDisconnect:
            pass
        finally:
            inst.unsubscribe(q)

    # --- Diff / Settings / Plans / Memory endpoints -----------------------

    def _require_instance(title: str) -> Instance:
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        return inst

    def _rules_file_specs(inst: Instance, workdir: Path) -> list[tuple[str, Path]]:
        if inst.provider == "codex":
            return [
                ("AGENTS.md", workdir / "AGENTS.md"),
                ("~/.codex/config.toml", Path.home() / ".codex" / "config.toml"),
                (".mcp.json", workdir / ".mcp.json"),
            ]
        return [
            ("CLAUDE.md", workdir / "CLAUDE.md"),
            (".claude/settings.json", workdir / ".claude" / "settings.json"),
            (".claude/settings.local.json", workdir / ".claude" / "settings.local.json"),
            (".mcp.json", workdir / ".mcp.json"),
        ]

    def _write_rules_file(inst: Instance, workdir: Path, requested_path: str, content: str) -> Path:
        requested = Path(requested_path).expanduser().resolve()
        allowed_absolute_paths = {
            path.expanduser().resolve()
            for _, path in _rules_file_specs(inst, workdir)
            if path.is_absolute()
        }
        if requested in allowed_absolute_paths:
            requested.parent.mkdir(parents=True, exist_ok=True)
            requested.write_text(content, encoding="utf-8")
            return requested
        return write_file_under(workdir, requested_path, content)

    @app.get("/api/instances/{title}/diff")
    async def get_diff(title: str) -> dict[str, Any]:
        inst = _require_instance(title)
        rc, stdout, stderr = await run_git(Path(inst.path), "diff")
        return {"content": stdout, "error": stderr if rc != 0 else None, "returncode": rc}

    @app.get("/api/instances/{title}/git-status")
    async def get_git_status(title: str) -> dict[str, Any]:
        inst = _require_instance(title)
        check_rc, _, _ = await run_git(Path(inst.path), "rev-parse", "--git-dir")
        if check_rc != 0:
            return {"is_git": False, "branch": "", "status": ""}
        _, branch, _ = await run_git(Path(inst.path), "branch", "--show-current")
        _, status, _ = await run_git(Path(inst.path), "status", "--short")
        return {"is_git": True, "branch": branch.strip(), "status": status}

    @app.get("/api/instances/{title}/rules")
    async def get_rules(title: str) -> dict[str, Any]:
        inst = _require_instance(title)
        workdir = Path(inst.path)
        specs = _rules_file_specs(inst, workdir)
        return {"files": read_named_files(specs)}

    @app.put("/api/instances/{title}/rules")
    async def put_rules(title: str, body: FileWriteBody) -> dict[str, Any]:
        inst = _require_instance(title)
        workdir = Path(inst.path)
        try:
            target = _write_rules_file(inst, workdir, body.path, body.content)
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        except OSError as e:
            raise HTTPException(status_code=500, detail=str(e))
        return {"ok": True, "path": str(target)}

    @app.get("/api/instances/{title}/plans")
    async def get_plans(title: str) -> dict[str, Any]:
        inst = _require_instance(title)
        plans_dir = Path(inst.path) / ".claude" / "plans"
        return {"files": read_md_files(plans_dir), "directory": str(plans_dir)}

    @app.put("/api/instances/{title}/plans")
    async def put_plans(title: str, body: FileWriteBody) -> dict[str, Any]:
        inst = _require_instance(title)
        plans_dir = Path(inst.path) / ".claude" / "plans"
        plans_dir.mkdir(parents=True, exist_ok=True)
        try:
            target = write_file_under(plans_dir, body.path, body.content)
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        except OSError as e:
            raise HTTPException(status_code=500, detail=str(e))
        return {"ok": True, "path": str(target)}

    @app.get("/api/instances/{title}/memory")
    async def get_memory(title: str) -> dict[str, Any]:
        inst = _require_instance(title)
        mem_dir = memory_dir_for(inst.path)
        return {"files": read_md_files(mem_dir), "directory": str(mem_dir)}

    @app.put("/api/instances/{title}/memory")
    async def put_memory(title: str, body: FileWriteBody) -> dict[str, Any]:
        inst = _require_instance(title)
        mem_dir = memory_dir_for(inst.path)
        mem_dir.mkdir(parents=True, exist_ok=True)
        try:
            target = write_file_under(mem_dir, body.path, body.content)
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        except OSError as e:
            raise HTTPException(status_code=500, detail=str(e))
        return {"ok": True, "path": str(target)}

    # --- Auth endpoints ---------------------------------------------------

    @app.get("/api/providers/{provider}/auth/status")
    async def provider_auth_status(provider: str) -> dict[str, Any]:
        return _provider_auth(provider).provider_status()

    @app.post("/api/providers/{provider}/auth/login", status_code=201)
    async def provider_auth_login_start(provider: str) -> dict[str, Any]:
        auth_registry = _provider_auth(provider)
        try:
            session = await auth_registry.start()
        except RuntimeError as e:
            raise HTTPException(status_code=501, detail=str(e))
        return {"id": session.id, "provider": provider}

    @app.post("/api/providers/{provider}/auth/login/{sid}/input")
    async def provider_auth_login_input(provider: str, sid: str, body: InputBody) -> dict[str, Any]:
        session = _provider_auth(provider).get(sid)
        if not session:
            raise HTTPException(status_code=404)
        try:
            await session.write_input(body.data)
        except RuntimeError as e:
            raise HTTPException(status_code=409, detail=str(e))
        return {"ok": True}

    @app.delete("/api/providers/{provider}/auth/login/{sid}", status_code=204)
    async def provider_auth_login_cancel(provider: str, sid: str) -> Response:
        auth_registry = _provider_auth(provider)
        if auth_registry.get(sid) is None:
            raise HTTPException(status_code=404)
        await auth_registry.close(sid)
        return Response(status_code=204)

    @app.websocket("/api/providers/{provider}/auth/login/{sid}")
    async def provider_auth_login_ws(ws: WebSocket, provider: str, sid: str) -> None:
        try:
            auth_registry = _provider_auth(provider)
        except HTTPException:
            await ws.close(code=1008, reason="unknown provider")
            return
        session = auth_registry.get(sid)
        if not session:
            await ws.close(code=1008, reason="login session not found")
            return
        await ws.accept()
        for event in session.history():
            await ws.send_text(json.dumps(event))
        if session.done:
            return
        q = session.subscribe()
        try:
            while True:
                event = await q.get()
                await ws.send_text(json.dumps(event))
                if event.get("type") == "done":
                    break
        except WebSocketDisconnect:
            pass
        finally:
            session.unsubscribe(q)

    @app.get("/api/auth/status")
    async def auth_status() -> dict[str, Any]:
        return _provider_auth("claude").provider_status()

    @app.post("/api/auth/login", status_code=201)
    async def auth_login_start() -> dict[str, Any]:
        return await provider_auth_login_start("claude")

    @app.post("/api/auth/login/{sid}/input")
    async def auth_login_input(sid: str, body: InputBody) -> dict[str, Any]:
        return await provider_auth_login_input("claude", sid, body)

    @app.delete("/api/auth/login/{sid}", status_code=204)
    async def auth_login_cancel(sid: str) -> Response:
        return await provider_auth_login_cancel("claude", sid)

    @app.websocket("/api/auth/login/{sid}")
    async def auth_login_ws(ws: WebSocket, sid: str) -> None:
        session = auth.get(sid)
        if not session:
            await ws.close(code=1008, reason="login session not found")
            return
        await ws.accept()
        for event in session.history():
            await ws.send_text(json.dumps(event))
        if session.done:
            return
        q = session.subscribe()
        try:
            while True:
                event = await q.get()
                await ws.send_text(json.dumps(event))
                if event.get("type") == "done":
                    break
        except WebSocketDisconnect:
            pass
        finally:
            session.unsubscribe(q)

    # --- Static files -----------------------------------------------------

    static_dir = _find_static_dir()
    if static_dir:
        log.info("serving static files from %s", static_dir)
        index_html = static_dir / "index.html"

        # SPA tabs for client-side routing
        _SPA_TABS = {"conversation", "diff", "settings", "plans", "memory"}

        def _is_spa_route(path: str) -> bool:
            """Check if path is a client-side SPA route."""
            parts = path.strip("/").split("/")
            if len(parts) == 0 or parts[0] == "":
                return False
            # /{instance} - single segment, no dots (not a file)
            if len(parts) == 1:
                return "." not in parts[0] and parts[0] != "api"
            # /{instance}/{tab} - two segments, tab must be valid
            if len(parts) == 2:
                return parts[1] in _SPA_TABS
            return False

        # Middleware to handle SPA routes - runs AFTER StaticFiles would 404
        from starlette.middleware.base import BaseHTTPMiddleware
        from starlette.types import ASGIApp

        class SPAMiddleware(BaseHTTPMiddleware):
            async def dispatch(self, request: Request, call_next):
                response = await call_next(request)
                # If StaticFiles returned 404 and path looks like SPA route, serve index.html
                if response.status_code == 404 and _is_spa_route(request.url.path):
                    return Response(
                        content=index_html.read_text(encoding="utf-8"),
                        media_type="text/html",
                        headers={"Cache-Control": "no-store"},
                    )
                # Prevent browsers from caching static assets (HTML, CSS, JS).
                # Skip /api/ routes — they manage their own headers and include
                # streaming responses where mutating headers after the fact is unsafe.
                if not request.url.path.startswith("/api/"):
                    response.headers["Cache-Control"] = "no-store"
                return response

        app.add_middleware(SPAMiddleware)
        app.mount("/", StaticFiles(directory=str(static_dir), html=True), name="static")
    else:
        log.error(
            "static directory not found (tried AGENT_MANAGER_STATIC_DIR, module dir, /app/static, CWD); "
            "API still works at /api/*"
        )

    return app
