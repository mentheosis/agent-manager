from __future__ import annotations

import json
import logging
import os
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

from fastapi import FastAPI, HTTPException, WebSocket, WebSocketDisconnect
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
from .persistence import Persistence
from .state import Registry

log = logging.getLogger(__name__)


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
    add_dirs: list[str] | None = None


def _summary(inst: Instance) -> dict[str, Any]:
    return {
        "title": inst.title,
        "display_title": inst.display_title,
        "path": inst.path,
        "permission_mode": inst.permission_mode,
        "status": inst.status,
        "created_at": inst.created_at,
        "add_dirs": list(inst.add_dirs or []),
    }


def build_app() -> FastAPI:
    persistence = Persistence()
    persistence.ensure_dirs()
    registry = Registry(persistence)
    auth = AuthRegistry()

    @asynccontextmanager
    async def lifespan(app: FastAPI):
        try:
            await registry.load_from_disk()
        except Exception:
            log.exception("failed to load persisted state; continuing with empty registry")
        try:
            yield
        finally:
            await registry.shutdown()
            await auth.shutdown()

    app = FastAPI(title="agent-manager", version="0.1.0", lifespan=lifespan)
    app.state.registry = registry
    app.state.auth = auth
    app.state.persistence = persistence

    @app.get("/api/instances")
    async def list_instances() -> list[dict[str, Any]]:
        return [_summary(i) for i in registry.list()]

    @app.post("/api/instances", status_code=201)
    async def create_instance(body: CreateInstanceBody) -> dict[str, Any]:
        try:
            inst = await registry.create(
                body.name, body.path, body.permission_mode, body.add_dirs
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
            add_dirs=body.add_dirs,
        )
        if inst is None:
            raise HTTPException(status_code=404)
        return _summary(inst)

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
    async def events_ws(ws: WebSocket, title: str) -> None:
        inst = registry.get(title)
        if not inst:
            await ws.close(code=1008, reason="instance not found")
            return
        await ws.accept()
        for event in inst.history():
            await ws.send_text(json.dumps(event))
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
        app.mount("/", StaticFiles(directory=str(static_dir), html=True), name="static")
    else:
        log.error(
            "static directory not found (tried AGENT_MANAGER_STATIC_DIR, module dir, /app/static, CWD); "
            "API still works at /api/*"
        )

    return app
