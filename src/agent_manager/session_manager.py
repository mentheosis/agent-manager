"""Session management: context-pressure monitoring and split decisions.

Pure logic, no SDK import — this is the Python port of the Go `sessionmgr`
package's `config.go` + `monitor.go` (the most-tested part), carrying over its
correctness fixes:

  * Splits fire on a **live-revalidated** reading, not a sticky latch, so a flag
    armed by an earlier high reading cannot fire once usage has dropped below the
    threshold (the "fired at 15%" bug).
  * A **post-split cooldown** suppresses auto-splits right after a handoff so a
    fresh session is not split again before it does real work (cascade), and
    failed-handoff retries back off.
  * A **manual split arms** and is honored regardless of context level or cooldown
    (explicit human action), fired by the coordinator when the agent is idle.

Unlike the tmux version there is no scrollback/compaction heuristic: this app has
real per-turn token `usage` from the SDK (`ResultMessage.usage`), so context-usage
is the only trigger source.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Callable

# Defaults (mirror sessionmgr/config.go).
DEFAULT_SOFT_CONTEXT_PERCENTAGE = 70
DEFAULT_HARD_CONTEXT_PERCENTAGE = 90
DEFAULT_CHECKPOINT_TIMEOUT_SEC = 180
DEFAULT_CONTEXT_WINDOW_SIZE = 200_000
DEFAULT_SPLIT_COOLDOWN_SEC = 60


@dataclass
class SessionConfig:
    """Per-instance session-management configuration (persisted alongside the instance)."""

    enabled: bool = False
    # A 0 on any of these means "use the default".
    soft_context_percentage: int = 0
    hard_context_percentage: int = 0
    checkpoint_timeout_sec: int = 0
    context_window_size: int = 0
    split_cooldown_sec: int = 0

    def effective_soft_pct(self) -> int:
        return self.soft_context_percentage or DEFAULT_SOFT_CONTEXT_PERCENTAGE

    def effective_hard_pct(self) -> int:
        return self.hard_context_percentage or DEFAULT_HARD_CONTEXT_PERCENTAGE

    def effective_checkpoint_timeout_sec(self) -> int:
        return self.checkpoint_timeout_sec or DEFAULT_CHECKPOINT_TIMEOUT_SEC

    def effective_context_window_size(self) -> int:
        return self.context_window_size or DEFAULT_CONTEXT_WINDOW_SIZE

    def effective_split_cooldown_sec(self) -> int:
        return self.split_cooldown_sec or DEFAULT_SPLIT_COOLDOWN_SEC

    def to_dict(self) -> dict[str, Any]:
        return {
            "enabled": self.enabled,
            "soft_context_percentage": self.soft_context_percentage,
            "hard_context_percentage": self.hard_context_percentage,
            "checkpoint_timeout_sec": self.checkpoint_timeout_sec,
            "context_window_size": self.context_window_size,
            "split_cooldown_sec": self.split_cooldown_sec,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any] | None) -> "SessionConfig":
        d = d or {}
        return cls(
            enabled=bool(d.get("enabled", False)),
            soft_context_percentage=int(d.get("soft_context_percentage", 0) or 0),
            hard_context_percentage=int(d.get("hard_context_percentage", 0) or 0),
            checkpoint_timeout_sec=int(d.get("checkpoint_timeout_sec", 0) or 0),
            context_window_size=int(d.get("context_window_size", 0) or 0),
            split_cooldown_sec=int(d.get("split_cooldown_sec", 0) or 0),
        )


@dataclass
class CheckpointMeta:
    session: int
    path: str
    trigger: str
    timestamp: str

    def to_dict(self) -> dict[str, Any]:
        return {"session": self.session, "path": self.path, "trigger": self.trigger, "timestamp": self.timestamp}

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> "CheckpointMeta":
        return cls(
            session=int(d.get("session", 0)),
            path=str(d.get("path", "")),
            trigger=str(d.get("trigger", "")),
            timestamp=str(d.get("timestamp", "")),
        )


@dataclass
class SessionState:
    """Per-instance handoff state (current segment number + checkpoint history)."""

    current_session: int = 1
    checkpoints: list[CheckpointMeta] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "current_session": self.current_session,
            "checkpoints": [c.to_dict() for c in self.checkpoints],
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any] | None) -> "SessionState":
        d = d or {}
        return cls(
            current_session=int(d.get("current_session", 1) or 1),
            checkpoints=[CheckpointMeta.from_dict(c) for c in d.get("checkpoints", []) or []],
        )


def usage_total_tokens(usage: dict[str, Any] | None) -> int:
    """Total context tokens from an SDK usage dict (input + both caches + output).

    Matches the Go reader and Claude Code's own indicator (which counts the
    current turn's output tokens, since they become cached input next turn).
    """
    if not usage:
        return 0
    return (
        int(usage.get("input_tokens", 0) or 0)
        + int(usage.get("cache_creation_input_tokens", 0) or 0)
        + int(usage.get("cache_read_input_tokens", 0) or 0)
        + int(usage.get("output_tokens", 0) or 0)
    )


class ContextMonitor:
    """Tracks context pressure and decides when a split should fire.

    Fed real token usage (from the SDK `result` event). The split decision is
    re-validated against the *live* reading at fire time and gated by a post-split
    cooldown; a manual request bypasses both.

    A monotonic `clock` is injectable so cooldown behaviour is deterministic in
    tests (no sleeping).
    """

    def __init__(self, config: SessionConfig, clock: Callable[[], float] = time.monotonic) -> None:
        self._soft_pct = config.effective_soft_pct()
        self._hard_pct = config.effective_hard_pct()
        self._cooldown_sec = config.effective_split_cooldown_sec()
        self._window = config.effective_context_window_size()
        self._clock = clock

        self._usage: dict[str, Any] | None = None
        self._used_pct: float = 0.0
        self._soft_triggered = False
        self._hard_triggered = False
        self._manual_requested = False
        self._cooldown_until: float = 0.0  # monotonic deadline; 0.0 means no cooldown active

    # --- ingest -------------------------------------------------------------

    def ingest_context_usage(self, usage: dict[str, Any] | None, window_size: int | None = None) -> None:
        """Update from an SDK usage dict. `window_size` overrides the configured one
        (e.g. resolved from the model on the `system_init` event)."""
        if usage is None:
            return
        window = window_size or self._window
        if window <= 0:
            window = DEFAULT_CONTEXT_WINDOW_SIZE
        total = usage_total_tokens(usage)
        pct = min(total / window * 100.0, 100.0)

        self._usage = usage
        self._used_pct = pct
        if pct >= self._soft_pct:
            self._soft_triggered = True
        if pct >= self._hard_pct:
            self._hard_triggered = True

    # --- live-revalidated split decisions -----------------------------------

    def _should_split_now(self, latched: bool, threshold_pct: int) -> bool:
        if not latched:
            return False
        if self.in_cooldown():
            return False
        # Re-validate against the live reading: a flag armed by an earlier high
        # reading must not fire once usage has dropped below the threshold.
        return self._used_pct >= threshold_pct

    def should_soft_split_now(self) -> bool:
        return self._should_split_now(self._soft_triggered, self._soft_pct)

    def should_hard_split_now(self) -> bool:
        return self._should_split_now(self._hard_triggered, self._hard_pct)

    # --- manual split (explicit human action; not gated) --------------------

    def request_manual_split(self) -> None:
        self._manual_requested = True

    def should_manual_split(self) -> bool:
        return self._manual_requested

    # --- fire-on-idle decision (precedence: hard > manual > soft) -----------

    def next_split_trigger(self) -> str | None:
        """The trigger string for a split that should fire now (called on the idle
        gate), or None. Hard takes precedence (it used the urgent wrap-up prompt),
        then an explicit manual request, then soft. Manual is honored regardless of
        the live reading or cooldown; soft/hard are live-revalidated + cooldown-gated.
        """
        if self.should_hard_split_now():
            return f"hard_context_{int(self._used_pct)}%"
        if self.should_manual_split():
            return "manual"
        if self.should_soft_split_now():
            return f"soft_context_{int(self._used_pct)}%"
        return None

    # --- cooldown -----------------------------------------------------------

    def in_cooldown(self) -> bool:
        return self._cooldown_until > 0.0 and self._clock() < self._cooldown_until

    # --- lifecycle ----------------------------------------------------------

    def reset(self) -> None:
        """Clear all flags after a handoff completes (or fails) and open the
        post-split cooldown window."""
        self._usage = None
        self._used_pct = 0.0
        self._soft_triggered = False
        self._hard_triggered = False
        self._manual_requested = False
        if self._cooldown_sec > 0:
            self._cooldown_until = self._clock() + self._cooldown_sec

    # --- introspection (for the UI / status endpoint) -----------------------

    def pressure(self) -> dict[str, Any]:
        total = usage_total_tokens(self._usage)
        return {
            "used_percentage": round(self._used_pct, 1),
            "total_context_tokens": total,
            "context_window_size": self._window,
            "soft_threshold": self._soft_pct,
            "hard_threshold": self._hard_pct,
            "should_soft_split": self.should_soft_split_now(),
            "should_hard_split": self.should_hard_split_now(),
            "in_cooldown": self.in_cooldown(),
            "manual_split_pending": self._manual_requested,
        }
