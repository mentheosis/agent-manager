from __future__ import annotations

import asyncio
import logging
import os
import pty
import signal
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)

AuthEvent = dict[str, Any]

CREDENTIALS_PATH = Path.home() / ".claude" / ".credentials.json"


@dataclass
class LoginSession:
    """One in-flight `claude login` subprocess.

    Runs under a pty so the CLI sees a TTY and behaves interactively.
    Output is broadcast to any number of WebSocket subscribers; stdin is
    written to when the UI posts a paste-back code.
    """

    id: str
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
                "claude",
                "login",
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
    def __init__(self) -> None:
        self._sessions: dict[str, LoginSession] = {}

    @staticmethod
    def is_authed() -> bool:
        return CREDENTIALS_PATH.exists()

    @staticmethod
    def credentials_path() -> str:
        return str(CREDENTIALS_PATH)

    async def start(self) -> LoginSession:
        session = LoginSession(id=str(uuid.uuid4()))
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
