from __future__ import annotations

import json
import logging
import os
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import httpx

from fastapi import FastAPI, HTTPException, Request, WebSocket, WebSocketDisconnect
from fastapi.responses import Response
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, Field

from .auth import AuthRegistry
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
from .state import Registry

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


class CreateInstanceBody(BaseModel):
    name: str = Field(min_length=1)
    path: str = Field(min_length=1)
    permission_mode: str = "acceptEdits"
    model: str | None = None
    add_dirs: list[str] = Field(default_factory=list)


class SendBody(BaseModel):
    text: str = Field(min_length=1)


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


class ReparentBody(BaseModel):
    parent: str | None = None  # New parent title, or None to remove from parent


class TaskBody(BaseModel):
    task: str | None = None


class InstanceTypeBody(BaseModel):
    instance_type: str | None = None  # "claude" | "loop"
    agent_preset: str | None = None  # "coder" | "researcher" | "orchestrator"


def _summary(inst: Instance) -> dict[str, Any]:
    return {
        "title": inst.title,
        "display_title": inst.display_title,
        "path": inst.path,
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
    }


_FALLBACK_MODELS = [
    "claude-opus-4-7",
    "claude-sonnet-4-7",
    "claude-opus-4-5",
    "claude-sonnet-4-5",
    "claude-haiku-3-5",
]

_models_cache: list[str] | None = None


async def _fetch_models() -> list[str]:
    """Fetch available models from the Anthropic API, caching after the first call."""
    global _models_cache
    if _models_cache is not None:
        return _models_cache
    api_key = os.environ.get("ANTHROPIC_API_KEY", "")
    if not api_key:
        log.info("ANTHROPIC_API_KEY not set; using fallback model list")
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
        log.warning("failed to fetch models from Anthropic API; using fallback list", exc_info=True)
    return _FALLBACK_MODELS


def build_app() -> FastAPI:
    persistence = Persistence()
    persistence.ensure_dirs()
    registry = Registry(persistence)
    auth = AuthRegistry()

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
        try:
            await _fetch_models()
        except Exception:
            log.exception("model pre-fetch failed; will retry on first /api/models request")
        try:
            yield
        finally:
            await orchestrator_manager.shutdown()
            await registry.shutdown()
            await auth.shutdown()

    app = FastAPI(title="agent-manager", version="0.1.0", lifespan=lifespan)
    app.state.registry = registry
    app.state.auth = auth
    app.state.persistence = persistence

    # Polling endpoints that should log at DEBUG instead of INFO
    _QUIET_PATHS = {"/api/auth/status", "/api/instances"}

    @app.middleware("http")
    async def access_log_middleware(request: Request, call_next):
        start = time.perf_counter()
        response = await call_next(request)
        duration_ms = (time.perf_counter() - start) * 1000
        path = request.url.path
        level = logging.DEBUG if (request.method == "GET" and path in _QUIET_PATHS) else logging.INFO
        log.log(level, '%s %s %s %.0fms', request.method, path, response.status_code, duration_ms)
        return response

    @app.get("/api/models")
    async def list_models() -> list[str]:
        return await _fetch_models()

    @app.get("/api/instances")
    async def list_instances() -> list[dict[str, Any]]:
        return [_summary(i) for i in registry.list()]

    @app.post("/api/instances", status_code=201)
    async def create_instance(body: CreateInstanceBody) -> dict[str, Any]:
        try:
            inst = await registry.create(
                body.name, body.path, body.permission_mode, body.model, body.add_dirs
            )
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        except FileNotFoundError as e:
            raise HTTPException(status_code=400, detail=f"path not found: {e}")
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
        if inst and inst.instance_type == "loop":
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
            model=body.model,
            add_dirs=body.add_dirs,
        )
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

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
                agent_preset=body.agent_preset,
            )
        except ValueError as e:
            raise HTTPException(status_code=400, detail=str(e))
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

    # --- Orchestrator process management --------------------------------------

    @app.post("/api/instances/{title}/orchestrator/start")
    async def start_orchestrator(title: str) -> dict[str, Any]:
        """Start the orchestrator process for a loop instance."""
        inst = registry.get(title)
        if not inst:
            raise HTTPException(status_code=404)
        if inst.instance_type != "loop":
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
        if inst.instance_type != "loop":
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
        await inst.send(body.text)
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
        specs = [
            ("CLAUDE.md", workdir / "CLAUDE.md"),
            (".claude/settings.json", workdir / ".claude" / "settings.json"),
            (".claude/settings.local.json", workdir / ".claude" / "settings.local.json"),
            (".mcp.json", workdir / ".mcp.json"),
        ]
        return {"files": read_named_files(specs)}

    @app.put("/api/instances/{title}/rules")
    async def put_rules(title: str, body: FileWriteBody) -> dict[str, Any]:
        inst = _require_instance(title)
        workdir = Path(inst.path)
        try:
            target = write_file_under(workdir, body.path, body.content)
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

    @app.get("/api/auth/status")
    async def auth_status() -> dict[str, Any]:
        return {
            "authed": AuthRegistry.is_authed(),
            "credentials_path": AuthRegistry.credentials_path(),
        }

    @app.post("/api/auth/login", status_code=201)
    async def auth_login_start() -> dict[str, Any]:
        session = await auth.start()
        return {"id": session.id}

    @app.post("/api/auth/login/{sid}/input")
    async def auth_login_input(sid: str, body: InputBody) -> dict[str, Any]:
        session = auth.get(sid)
        if not session:
            raise HTTPException(status_code=404)
        try:
            await session.write_input(body.data)
        except RuntimeError as e:
            raise HTTPException(status_code=409, detail=str(e))
        return {"ok": True}

    @app.delete("/api/auth/login/{sid}", status_code=204)
    async def auth_login_cancel(sid: str) -> Response:
        if auth.get(sid) is None:
            raise HTTPException(status_code=404)
        await auth.close(sid)
        return Response(status_code=204)

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
