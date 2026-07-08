from __future__ import annotations

import asyncio
import json
import logging
import os
import pty
import shutil
import signal
import subprocess
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

log = logging.getLogger(__name__)

AuthEvent = dict[str, Any]

CREDENTIALS_PATH = Path.home() / ".claude" / ".credentials.json"
CODEX_AUTH_PATH = Path.home() / ".codex" / "auth.json"
LOGIN_COMMAND = ("claude", "auth", "login")
CODEX_LOGIN_COMMAND = ("codex", "login")


def _parse_auth_status(raw: str) -> dict[str, Any]:
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        return {}
    return data if isinstance(data, dict) else {}


def _claude_auth_status() -> dict[str, Any]:
    """Return `claude auth status --json` output when the CLI is available."""
    if shutil.which("claude") is None:
        return {}
    try:
        proc = subprocess.run(
            ["claude", "auth", "status", "--json"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.TimeoutExpired):
        return {}
    return _parse_auth_status(proc.stdout)


def _codex_auth_status() -> dict[str, Any]:
    """Return best-effort Codex auth status.

    Codex is not installed in the current Docker image yet, so this deliberately
    degrades to an unauthenticated file check until Phase 4 wires the runtime.
    """
    if shutil.which("codex") is None:
        return {}
    try:
        proc = subprocess.run(
            ["codex", "login", "status"],
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.TimeoutExpired):
        return {}
    parsed = _parse_auth_status(proc.stdout)
    if parsed:
        return parsed
    if proc.returncode == 0 and "logged in" in proc.stdout.lower():
        return {"loggedIn": True, "authMethod": proc.stdout.strip()}
    return {}


@dataclass
class LoginSession:
    """One in-flight `claude login` subprocess.

    Runs under a pty so the CLI sees a TTY and behaves interactively.
    Output is broadcast to any number of WebSocket subscribers; stdin is
    written to when the UI posts a paste-back code.
    """

    id: str
    command: tuple[str, ...] = LOGIN_COMMAND
    _proc: asyncio.subprocess.Process | None = field(default=None, repr=False)
    _master_fd: int | None = field(default=None, repr=False)
    _history: list[AuthEvent] = field(default_factory=list, repr=False)
    _subscribers: list[asyncio.Queue[AuthEvent]] = field(default_factory=list, repr=False)
    _reader_task: asyncio.Task | None = field(default=None, repr=False)
    done: bool = False
    returncode: int | None = None

    async def start(self) -> None:
        master_fd, slave_fd = pty.openpty()
        self._master_fd = master_fd
        try:
            self._proc = await asyncio.create_subprocess_exec(
                *self.command,
                stdin=slave_fd,
                stdout=slave_fd,
                stderr=slave_fd,
                start_new_session=True,
            )
        finally:
            os.close(slave_fd)
        self._reader_task = asyncio.create_task(self._read_loop(), name=f"login-read:{self.id}")

    async def _read_loop(self) -> None:
        loop = asyncio.get_running_loop()
        assert self._master_fd is not None
        assert self._proc is not None
        try:
            while True:
                try:
                    data = await loop.run_in_executor(None, os.read, self._master_fd, 4096)
                except OSError:
                    break
                if not data:
                    break
                text = data.decode("utf-8", errors="replace")
                await self._publish({"type": "output", "text": text})
        finally:
            try:
                self.returncode = await self._proc.wait()
            except Exception:
                self.returncode = -1
            self.done = True
            await self._publish({"type": "done", "returncode": self.returncode})

    async def write_input(self, data: str) -> None:
        """Write arbitrary bytes to the subprocess stdin (no automatic newline).

        Caller is responsible for appending `\\r` / `\\n` or escape sequences
        (e.g. `\\x1b[A` for arrow up) as needed.
        """
        if self._master_fd is None:
            raise RuntimeError("login session not started")
        if self.done:
            raise RuntimeError("login session already finished")
        encoded = data.encode("utf-8")
        loop = asyncio.get_running_loop()
        await loop.run_in_executor(None, os.write, self._master_fd, encoded)

    async def stop(self) -> None:
        if self._proc is not None and self._proc.returncode is None:
            try:
                self._proc.send_signal(signal.SIGTERM)
                await asyncio.wait_for(self._proc.wait(), timeout=2.0)
            except (ProcessLookupError, asyncio.TimeoutError):
                try:
                    self._proc.kill()
                except ProcessLookupError:
                    pass
        if self._master_fd is not None:
            try:
                os.close(self._master_fd)
            except OSError:
                pass
            self._master_fd = None
        if self._reader_task and not self._reader_task.done():
            self._reader_task.cancel()
            try:
                await self._reader_task
            except (asyncio.CancelledError, Exception):
                pass

    def subscribe(self) -> asyncio.Queue[AuthEvent]:
        q: asyncio.Queue[AuthEvent] = asyncio.Queue()
        self._subscribers.append(q)
        return q

    def unsubscribe(self, q: asyncio.Queue[AuthEvent]) -> None:
        if q in self._subscribers:
            self._subscribers.remove(q)

    def history(self) -> list[AuthEvent]:
        return list(self._history)

    async def _publish(self, event: AuthEvent) -> None:
        self._history.append(event)
        for q in list(self._subscribers):
            q.put_nowait(event)


class AuthRegistry:
    def __init__(
        self,
        *,
        provider: str = "claude",
        login_command: tuple[str, ...] = LOGIN_COMMAND,
        credentials_path: Path = CREDENTIALS_PATH,
        status_func: Callable[[], dict[str, Any]] | None = None,
        login_supported: bool = True,
    ) -> None:
        self.provider = provider
        self.login_command = login_command
        self.credentials_path = credentials_path
        self.status_func = status_func or _claude_auth_status
        self.login_supported = login_supported
        self._sessions: dict[str, LoginSession] = {}

    @staticmethod
    def is_authed() -> bool:
        status = _claude_auth_status()
        if "loggedIn" in status:
            return bool(status.get("loggedIn"))
        return CREDENTIALS_PATH.exists()

    @staticmethod
    def status() -> dict[str, Any]:
        return claude_auth_status()

    @staticmethod
    def credentials_path() -> str:
        return str(CREDENTIALS_PATH)

    async def start(self) -> LoginSession:
        if not self.login_supported:
            raise RuntimeError(f"{self.provider} login is not supported yet")
        if shutil.which(self.login_command[0]) is None:
            raise RuntimeError(f"{self.login_command[0]} CLI is not installed")
        session = LoginSession(id=str(uuid.uuid4()), command=self.login_command)
        await session.start()
        self._sessions[session.id] = session
        return session

    def get(self, sid: str) -> LoginSession | None:
        return self._sessions.get(sid)

    async def close(self, sid: str) -> None:
        session = self._sessions.pop(sid, None)
        if session is not None:
            await session.stop()

    async def shutdown(self) -> None:
        sessions = list(self._sessions.values())
        self._sessions.clear()
        for session in sessions:
            await session.stop()

    def provider_status(self) -> dict[str, Any]:
        return _status_payload(
            provider=self.provider,
            status=self.status_func(),
            credentials_path=self.credentials_path,
            login_supported=self.login_supported,
        )


def _status_payload(
    *,
    provider: str,
    status: dict[str, Any],
    credentials_path: Path,
    login_supported: bool,
) -> dict[str, Any]:
    credentials_exist = credentials_path.exists()
    # Some provider CLIs can briefly report loggedIn=false after a container
    # rebuild even though their persisted credentials are present and usable.
    # Treat either positive CLI status or existing credentials as authenticated;
    # provider turns remain the final authority for expired/invalid credentials.
    authed = bool(status.get("loggedIn")) or credentials_exist
    return {
        "provider": provider,
        "authed": authed,
        "credentials_path": str(credentials_path),
        "credentials_present": credentials_exist,
        "auth_method": status.get("authMethod") or status.get("auth_method"),
        "api_provider": status.get("apiProvider") or status.get("api_provider"),
        "login_supported": login_supported,
    }


def claude_auth_status() -> dict[str, Any]:
    return _status_payload(
        provider="claude",
        status=_claude_auth_status(),
        credentials_path=CREDENTIALS_PATH,
        login_supported=True,
    )


def codex_auth_status() -> dict[str, Any]:
    return _status_payload(
        provider="codex",
        status=_codex_auth_status(),
        credentials_path=CODEX_AUTH_PATH,
        login_supported=False,
    )


def codex_auth_raw_status() -> dict[str, Any]:
    return _codex_auth_status()
