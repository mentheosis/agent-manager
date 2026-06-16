"""Tests for agent_manager.session_manager (pure-logic port of the Go monitor).

Runnable two ways:
  * pytest:    pytest tests/test_session_manager.py
  * standalone: PYTHONPATH=src python3 tests/test_session_manager.py
"""

from __future__ import annotations

from agent_manager.session_manager import (
    DEFAULT_CHECKPOINT_TIMEOUT_SEC,
    DEFAULT_HARD_CONTEXT_PERCENTAGE,
    DEFAULT_SOFT_CONTEXT_PERCENTAGE,
    DEFAULT_SPLIT_COOLDOWN_SEC,
    ContextMonitor,
    SessionConfig,
    SessionState,
    usage_total_tokens,
)

WINDOW = 200_000


class FakeClock:
    """Deterministic monotonic clock for cooldown tests."""

    def __init__(self, t: float = 1000.0) -> None:
        self.t = t

    def __call__(self) -> float:
        return self.t

    def advance(self, dt: float) -> None:
        self.t += dt


def _usage(pct: float, window: int = WINDOW) -> dict:
    return {"input_tokens": int(window * pct / 100.0), "output_tokens": 0,
            "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}


def test_config_effective_defaults():
    c = SessionConfig()
    assert c.effective_soft_pct() == DEFAULT_SOFT_CONTEXT_PERCENTAGE
    assert c.effective_hard_pct() == DEFAULT_HARD_CONTEXT_PERCENTAGE
    assert c.effective_checkpoint_timeout_sec() == DEFAULT_CHECKPOINT_TIMEOUT_SEC
    assert c.effective_split_cooldown_sec() == DEFAULT_SPLIT_COOLDOWN_SEC
    # custom values win
    c2 = SessionConfig(soft_context_percentage=60, split_cooldown_sec=30)
    assert c2.effective_soft_pct() == 60
    assert c2.effective_split_cooldown_sec() == 30


def test_config_roundtrip():
    c = SessionConfig(enabled=True, soft_context_percentage=65, hard_context_percentage=88,
                      checkpoint_timeout_sec=120, context_window_size=1_000_000, split_cooldown_sec=45)
    assert SessionConfig.from_dict(c.to_dict()) == c


def test_usage_total_tokens():
    assert usage_total_tokens(None) == 0
    assert usage_total_tokens({"input_tokens": 100, "cache_read_input_tokens": 50,
                               "cache_creation_input_tokens": 20, "output_tokens": 30}) == 200


def test_soft_split_live_revalidation():
    """The 'fired at 15%' regression: a latched flag must NOT fire once the live
    reading has dropped below the threshold."""
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, split_cooldown_sec=1))
    cm.ingest_context_usage(_usage(81))
    assert cm.should_soft_split_now() is True  # latched and live still high

    cm.ingest_context_usage(_usage(15))  # e.g. a fresh, low-context session
    assert cm.should_soft_split_now() is False  # live reading no longer justifies it


def test_hard_split_live_revalidation():
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, hard_context_percentage=90))
    cm.ingest_context_usage(_usage(92))
    assert cm.should_hard_split_now() is True
    cm.ingest_context_usage(_usage(20))
    assert cm.should_hard_split_now() is False


def test_cooldown_suppresses_then_allows():
    clock = FakeClock()
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, split_cooldown_sec=60), clock=clock)

    cm.reset()  # simulate a completed handoff -> opens cooldown
    assert cm.in_cooldown() is True

    cm.ingest_context_usage(_usage(95))  # re-arms immediately (heavy restore)
    assert cm.should_soft_split_now() is False  # cooldown suppresses (cascade prevention)

    clock.advance(61)
    assert cm.in_cooldown() is False
    assert cm.should_soft_split_now() is True  # after cooldown, live reading still high


def test_no_cooldown_before_first_split():
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, split_cooldown_sec=300))
    assert cm.in_cooldown() is False
    cm.ingest_context_usage(_usage(85))
    assert cm.should_soft_split_now() is True  # first split is never blocked by a cooldown


def test_manual_split_arm_independent_of_threshold_and_cooldown():
    clock = FakeClock()
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, split_cooldown_sec=300), clock=clock)

    cm.ingest_context_usage(_usage(5))  # well below threshold
    cm.request_manual_split()
    assert cm.should_manual_split() is True            # armed despite low context
    assert cm.should_soft_split_now() is False         # context-based still says no

    cm.reset()                                         # opens cooldown, clears manual flag
    assert cm.should_manual_split() is False
    cm.request_manual_split()
    assert cm.should_manual_split() is True            # honored even during cooldown


def test_pressure_shape():
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70))
    cm.ingest_context_usage(_usage(50))
    p = cm.pressure()
    assert p["used_percentage"] == 50.0
    assert p["soft_threshold"] == 70
    assert p["manual_split_pending"] is False
    assert set(["used_percentage", "total_context_tokens", "context_window_size",
                "soft_threshold", "hard_threshold", "in_cooldown"]).issubset(p)


def test_next_split_trigger_precedence():
    # hard wins over soft when both are armed and live reading is high
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, hard_context_percentage=90))
    cm.ingest_context_usage(_usage(92))
    assert cm.next_split_trigger() == "hard_context_92%"

    # soft-only at 75%
    cm2 = ContextMonitor(SessionConfig(soft_context_percentage=70, hard_context_percentage=90))
    cm2.ingest_context_usage(_usage(75))
    assert cm2.next_split_trigger() == "soft_context_75%"

    # manual wins over (absent) soft at low context
    cm3 = ContextMonitor(SessionConfig(soft_context_percentage=70))
    cm3.ingest_context_usage(_usage(10))
    assert cm3.next_split_trigger() is None
    cm3.request_manual_split()
    assert cm3.next_split_trigger() == "manual"

    # nothing armed → None
    cm4 = ContextMonitor(SessionConfig(soft_context_percentage=70))
    cm4.ingest_context_usage(_usage(5))
    assert cm4.next_split_trigger() is None


def test_update_config_changes_thresholds():
    cm = ContextMonitor(SessionConfig(soft_context_percentage=70, hard_context_percentage=90))
    cm.ingest_context_usage(_usage(75))
    assert cm.should_soft_split_now() is True   # 75 >= 70
    # Raise the soft threshold above the live reading; the latch is re-validated live.
    cm.update_config(SessionConfig(soft_context_percentage=80, hard_context_percentage=95))
    assert cm.should_soft_split_now() is False  # 75 < 80 now
    p = cm.pressure()
    assert p["soft_threshold"] == 80 and p["hard_threshold"] == 95


def test_session_state_roundtrip():
    st = SessionState(current_session=3)
    st.checkpoints.append(__import__("agent_manager.session_manager", fromlist=["CheckpointMeta"]).CheckpointMeta(
        session=2, path="/x/checkpoint-2.md", trigger="manual", timestamp="2026-06-15T00:00:00Z"))
    rt = SessionState.from_dict(st.to_dict())
    assert rt.current_session == 3
    assert rt.checkpoints[0].session == 2 and rt.checkpoints[0].trigger == "manual"


if __name__ == "__main__":
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    passed = 0
    for fn in fns:
        fn()
        print(f"  PASS {fn.__name__}")
        passed += 1
    print(f"\n{passed}/{len(fns)} tests passed")
