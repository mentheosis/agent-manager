"""Async session-handoff coordinator.

Python port of the Go `sessionmgr/manager.go` `executeHandoff` state machine,
re-expressed as an asyncio task instead of a goroutine + polling. It owns the
*execution* half of a split (wrap-up → await fresh checkpoint → respawn fresh →
restore); the *decision* half (idle gate + threshold/manual checks) lives in the
event-driven wiring on the Instance/Registry, exactly as `pollMetadata` did in Go.

The split resets context by respawning the agent into a **fresh** session (no
`resume`), so the new conversation starts at ~0 tokens and rehydrates from the
checkpoint the previous session wrote. The respawn is gated on a checkpoint whose
mtime is strictly newer than a baseline captured before the wrap-up prompt — a
stale file from a prior aborted attempt can never trigger a respawn.
"""

from __future__ import annotations

import asyncio
import datetime as dt
import enum
import logging
from dataclasses import dataclass, field
from typing import Awaitable, Callable

from . import checkpoint
from .session_manager import CheckpointMeta, SessionConfig, SessionState

log = logging.getLogger(__name__)


class HandoffPhase(enum.IntEnum):
    IDLE = 0
    WRAP_UP = 1      # wrap-up prompt sent, waiting for checkpoint
    TRANSITION = 2   # checkpoint received, respawning fresh session
    RESTORE = 3      # new session started, sending restore prompt


@dataclass
class HandoffStatus:
    phase: HandoffPhase
    trigger: str
    started_at: str
    completed_at: str | None = None
    error: str | None = None


@dataclass
class HandoffCallbacks:
    """How the coordinator drives the instance it manages."""

    # Send a prompt to the agent (wrap-up / restore). Awaitable.
    send_prompt: Callable[[str], Awaitable[None]]
    # Respawn the agent into a FRESH session (no resume → context reset). Awaitable.
    respawn_fresh: Callable[[], Awaitable[None]]
    # Current instance status string ("ready" when idle and awaiting input).
    get_status: Callable[[], str]
    # Persist updated session state (optional).
    on_save: Callable[[SessionState], Awaitable[None]] | None = None


class HandoffCoordinator:
    def __init__(
        self,
        instance_title: str,
        config: SessionConfig,
        state: SessionState,
        callbacks: HandoffCallbacks,
        *,
        poll_interval_sec: float = 2.0,
        ready_timeout_sec: float = 30.0,
        sleep: Callable[[float], Awaitable[None]] = asyncio.sleep,
        now: Callable[[], dt.datetime] | None = None,
    ) -> None:
        self.title = instance_title
        self.config = config
        self.state = state
        self.cb = callbacks
        self._poll = poll_interval_sec
        self._ready_timeout = ready_timeout_sec
        self._sleep = sleep
        self._now = now or (lambda: dt.datetime.now(dt.timezone.utc))
        self._lock = asyncio.Lock()
        self._handoff: HandoffStatus | None = None
        self._task: asyncio.Task | None = None

    def handoff_in_progress(self) -> bool:
        h = self._handoff
        return h is not None and h.phase != HandoffPhase.IDLE

    def status(self) -> HandoffStatus | None:
        return self._handoff

    async def trigger_split(self, trigger: str) -> None:
        """Start a handoff in the background. Raises if one is already in progress."""
        if self.handoff_in_progress():
            raise RuntimeError(f"handoff already in progress for {self.title}")
        session_num = self.state.current_session
        self._handoff = HandoffStatus(
            phase=HandoffPhase.WRAP_UP, trigger=trigger, started_at=self._now().isoformat()
        )
        self._task = asyncio.create_task(
            self._execute_handoff(session_num, trigger), name=f"handoff:{self.title}"
        )

    async def wait(self) -> None:
        """Await the in-flight handoff task (used by callers/tests)."""
        if self._task is not None:
            await self._task

    def _fail(self, msg: str) -> None:
        log.error("[handoff %s] failed: %s", self.title, msg)
        if self._handoff is not None:
            self._handoff.phase = HandoffPhase.IDLE
            self._handoff.error = msg
            self._handoff.completed_at = self._now().isoformat()

    async def _execute_handoff(self, session_num: int, trigger: str) -> None:
        try:
            checkpoint.ensure_session_dir(self.title)

            is_hard = trigger.startswith("hard_")
            prompt = (
                checkpoint.hard_wrap_up_prompt(self.title, session_num)
                if is_hard
                else checkpoint.wrap_up_prompt(self.title, session_num)
            )

            # Capture the checkpoint's current mtime BEFORE sending the prompt, so we
            # only accept a file written *after* this baseline (stale-file guard).
            baseline_ns, had = checkpoint.checkpoint_mtime_ns(self.title, session_num)
            if had:
                log.info(
                    "[handoff %s] checkpoint for session %d already exists; requiring a newer write",
                    self.title, session_num,
                )

            await self.cb.send_prompt(prompt)

            timeout = float(self.config.effective_checkpoint_timeout_sec())
            if not await self._wait_for_fresh_checkpoint(session_num, baseline_ns, timeout):
                log.warning("[handoff %s] no fresh checkpoint after %.0fs, retrying wrap-up", self.title, timeout)
                await self.cb.send_prompt(prompt)
                if not await self._wait_for_fresh_checkpoint(session_num, baseline_ns, timeout):
                    self._fail("checkpoint not created or not updated after two attempts")
                    return

            # Phase 2: respawn into a fresh session (context reset).
            self._handoff.phase = HandoffPhase.TRANSITION  # type: ignore[union-attr]
            log.info("[handoff %s] checkpoint written, respawning fresh session", self.title)
            await self.cb.respawn_fresh()

            if not await self._wait_for_ready(self._ready_timeout):
                log.warning("[handoff %s] new session not ready within timeout, proceeding", self.title)

            # Phase 3: advance state and send the restore prompt to the new session.
            new_session = session_num + 1
            self._handoff.phase = HandoffPhase.RESTORE  # type: ignore[union-attr]
            self.state.current_session = new_session
            self.state.checkpoints.append(
                CheckpointMeta(
                    session=session_num,
                    path=str(checkpoint.checkpoint_path(self.title, session_num)),
                    trigger=trigger,
                    timestamp=self._now().isoformat(),
                )
            )
            if self.cb.on_save is not None:
                try:
                    await self.cb.on_save(self.state)
                except Exception:
                    log.exception("[handoff %s] failed to persist session state", self.title)

            await self.cb.send_prompt(checkpoint.restore_prompt(self.title, session_num))

            self._handoff.phase = HandoffPhase.IDLE  # type: ignore[union-attr]
            self._handoff.completed_at = self._now().isoformat()  # type: ignore[union-attr]
            log.info("[handoff %s] complete: session %d → %d", self.title, session_num, new_session)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            self._fail(f"{type(e).__name__}: {e}")

    async def _wait_for_fresh_checkpoint(self, session_num: int, baseline_ns: int, timeout: float) -> bool:
        """Poll until a checkpoint with mtime strictly after baseline_ns appears, or timeout."""
        elapsed = 0.0
        while elapsed < timeout:
            mtime_ns, exists = checkpoint.checkpoint_mtime_ns(self.title, session_num)
            if exists and mtime_ns > baseline_ns:
                return True
            await self._sleep(self._poll)
            elapsed += self._poll
        return False

    async def _wait_for_ready(self, timeout: float) -> bool:
        elapsed = 0.0
        while elapsed < timeout:
            if self.cb.get_status() == "ready":
                return True
            await self._sleep(1.0)
            elapsed += 1.0
        return False
