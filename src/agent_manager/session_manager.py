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

import logging
import time
from dataclasses import dataclass, field
from typing import Any, Callable

log = logging.getLogger(__name__)

# Defaults (mirror sessionmgr/config.go). Soft/hard sit high on purpose: a split
# is disruptive (checkpoint + respawn), so we only do it when context is genuinely
# near exhaustion. Low defaults were the root cause of the checkpoint/respawn loop —
# they fire a split on a session that has barely started working.
DEFAULT_SOFT_CONTEXT_PERCENTAGE = 85
DEFAULT_HARD_CONTEXT_PERCENTAGE = 95
DEFAULT_CHECKPOINT_TIMEOUT_SEC = 180
DEFAULT_CONTEXT_WINDOW_SIZE = 200_000
DEFAULT_SPLIT_COOLDOWN_SEC = 60

# Anti-loop floor: the smallest absolute token budget at which an auto-split may
# fire. A configured threshold below this is *unsatisfiable* — e.g. hard=2% on a
# 200K window = 4,000 tokens, but a fresh session's system prompt + tool schemas
# alone already exceed that. The split would re-fire on the very first `result`
# of every fresh session, checkpoint + respawn, and instantly be over the line
# again: an infinite checkpoint/respawn loop that wipes the agent's memory each
# pass. Below the floor we refuse to AUTO-split (manual splits are still honored),
# so a misconfiguration degrades to "session management does nothing" instead of
# a memory-wiping loop. This guards the persisted config on restart no matter how
# it was set, so it cannot be bypassed the way client-side validation can.
MIN_SPLIT_THRESHOLD_TOKENS = 30_000


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
    """Per-instance handoff state (current segment number + checkpoint history).

    Also carries the last context-pressure reading so the UI bar can show the last
    known occupancy immediately after a restart, instead of sitting at 0% until the
    agent emits its first new turn. These are display-only and updated on every ingest.
    """

    current_session: int = 1
    checkpoints: list[CheckpointMeta] = field(default_factory=list)
    last_used_pct: float = 0.0
    last_total_tokens: int = 0
    last_window: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "current_session": self.current_session,
            "checkpoints": [c.to_dict() for c in self.checkpoints],
            "last_used_pct": self.last_used_pct,
            "last_total_tokens": self.last_total_tokens,
            "last_window": self.last_window,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any] | None) -> "SessionState":
        d = d or {}
        return cls(
            current_session=int(d.get("current_session", 1) or 1),
            checkpoints=[CheckpointMeta.from_dict(c) for c in d.get("checkpoints", []) or []],
            last_used_pct=float(d.get("last_used_pct", 0.0) or 0.0),
            last_total_tokens=int(d.get("last_total_tokens", 0) or 0),
            last_window=int(d.get("last_window", 0) or 0),
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
        self._floor_warned = False  # one-shot log guard for an unsatisfiable threshold

    # --- ingest -------------------------------------------------------------

    def ingest_context_usage(self, usage: dict[str, Any] | None, window_size: int | None = None) -> None:
        """Update from an SDK usage dict. `window_size` overrides the configured one
        (e.g. resolved from the model on the `system_init` event)."""
        if usage is None:
            return
        window = window_size or self._window
        if window <= 0:
            window = DEFAULT_CONTEXT_WINDOW_SIZE
        # Adopt the resolved window as the monitor's window of record. The caller
        # passes a model-aware size (e.g. 1,000,000 for a [1m]/opus-4 model) that
        # supersedes the SessionConfig default; without this the percentage math
        # would use 1M while the floor guard, _threshold_tokens, and pressure()
        # still reported the stale 200K config default — an inconsistency that
        # under-counted the real headroom on 1M-context models.
        self._window = window
        total = usage_total_tokens(usage)
        pct = min(total / window * 100.0, 100.0)

        self._usage = usage
        self._used_pct = pct
        if pct >= self._soft_pct:
            self._soft_triggered = True
        if pct >= self._hard_pct:
            self._hard_triggered = True

    # --- live-revalidated split decisions -----------------------------------

    def _threshold_tokens(self, threshold_pct: int) -> float:
        """Absolute token budget the given threshold percentage corresponds to."""
        window = self._window if self._window > 0 else DEFAULT_CONTEXT_WINDOW_SIZE
        return threshold_pct / 100.0 * window

    def _should_split_now(self, latched: bool, threshold_pct: int) -> bool:
        if not latched:
            return False
        if self.in_cooldown():
            return False
        # Anti-loop floor: never auto-fire at a threshold a fresh session can't get
        # under (see MIN_SPLIT_THRESHOLD_TOKENS) — that guarantees an infinite
        # checkpoint/respawn loop. Refuse instead, warning once.
        if self._threshold_tokens(threshold_pct) < MIN_SPLIT_THRESHOLD_TOKENS:
            if not self._floor_warned:
                log.warning(
                    "session-mgmt: auto-split disabled — threshold %d%% of a %d-token "
                    "window is %d tokens, below the %d-token anti-loop floor (a fresh "
                    "session would re-trigger immediately). Raise the threshold or the "
                    "context window. Manual splits are still honored.",
                    threshold_pct, self._window, int(self._threshold_tokens(threshold_pct)),
                    MIN_SPLIT_THRESHOLD_TOKENS,
                )
                self._floor_warned = True
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

    def seed_pressure(self, used_pct: float, total_tokens: int, window: int) -> None:
        """Restore the last persisted reading so ``pressure()`` reflects the last known
        occupancy immediately after a (re)start, BEFORE the agent emits its first new
        turn (which is the only thing that produces a fresh live reading).

        Display-only: this deliberately does NOT arm the split latches. A restart must
        never fire a split from a stale reading — the latches re-arm naturally on the
        next live ``ingest_context_usage`` if still over threshold. The synthetic
        ``_usage`` makes ``usage_total_tokens`` report the restored token count so the
        pressure dict is internally consistent until the first live ingest overwrites it.
        """
        if total_tokens <= 0:
            return
        if window > 0:
            self._window = window
        self._used_pct = max(0.0, min(used_pct, 100.0))
        self._usage = {"input_tokens": int(total_tokens)}

    def update_config(self, config: "SessionConfig") -> None:
        """Apply new thresholds/window/cooldown on a live instance, then RE-EVALUATE
        the split latches against the new thresholds using the latest reading.

        The cooldown deadline is preserved (a config edit must not reopen a closed
        cooldown). The latches are NOT carried over verbatim: a previous version left
        ``_soft_triggered``/``_hard_triggered`` and the stale ``_used_pct`` untouched, so
        a latch armed under the OLD (low) thresholds — with ``_used_pct`` clamped at 100%
        against the OLD (small) window — would survive a threshold/window *raise* and
        fire one more split on the next idle gate. That defeats the exact action a user
        takes to STOP a runaway split loop (raise the threshold or the context window).
        The live-revalidation in ``_should_split_now`` does not catch it, because it
        compares the *stale* ``_used_pct`` (computed against the old window) to the new
        threshold. So here we recompute ``_used_pct`` from the stored usage against the
        new window and re-derive the latches from that live reading vs the new
        thresholds: raising the bar immediately disarms a now-unjustified latch, and
        lowering it arms one, as intended. Cooldown / floor / live-revalidation still
        gate any resulting split."""
        self._soft_pct = config.effective_soft_pct()
        self._hard_pct = config.effective_hard_pct()
        self._cooldown_sec = config.effective_split_cooldown_sec()
        self._window = config.effective_context_window_size()
        if self._usage is not None:
            window = self._window if self._window > 0 else DEFAULT_CONTEXT_WINDOW_SIZE
            self._used_pct = min(usage_total_tokens(self._usage) / window * 100.0, 100.0)
        self._soft_triggered = self._used_pct >= self._soft_pct
        self._hard_triggered = self._used_pct >= self._hard_pct

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
