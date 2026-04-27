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


class SendBody(BaseModel):
    text: str = Field(min_length=1)


class InputBody(BaseModel):
    data: str = Field(min_length=1)


class RenameBody(BaseModel):
    display_title: str | None = None


class ReorderBody(BaseModel):
    titles: list[str]


def _summary(inst: Instance) -> dict[str, Any]:
    return {
        "title": inst.title,
        "display_title": inst.display_title,
        "path": inst.path,
        "permission_mode": inst.permission_mode,
        "status": inst.status,
        "created_at": inst.created_at,
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
            inst = await registry.create(body.name, body.path, body.permission_mode)
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
