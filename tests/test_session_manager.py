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
    MIN_SPLIT_THRESHOLD_TOKENS,
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


def test_update_config_raise_window_disarms_stale_latch():
    """The tail of the real loop: a hard latch armed under a SMALL window (used_pct
    pegged at 100%) must NOT fire after the user raises the window + threshold live to
    escape the loop. The plain live-revalidation can't catch this — it compares the
    stale used_pct (100%, computed vs the old window) to the new threshold. update_config
    recomputes used_pct against the new window and re-derives the latch, so the
    now-unjustified latch is disarmed immediately, no restart required."""
    # Old config: small window + a threshold high enough to clear the anti-loop floor
    # (95% of 200K = 190K >> 30K floor), so the ONLY thing that could stop the split is
    # correct latch re-evaluation — not the floor.
    cm = ContextMonitor(SessionConfig(soft_context_percentage=85, hard_context_percentage=95,
                                      context_window_size=200_000))
    cm.ingest_context_usage(_usage(100, window=200_000))  # 200K tokens -> pegged at 100%
    assert cm.should_hard_split_now() is True              # armed and live vs old window

    # User raises BOTH the window and (re-affirms) the thresholds on the LIVE instance.
    cm.update_config(SessionConfig(soft_context_percentage=85, hard_context_percentage=95,
                                   context_window_size=1_000_000))
    # 200K tokens is only 20% of a 1M window -> far below 95%. The stale latch must die.
    assert cm.pressure()["used_percentage"] == 20.0        # recomputed vs the new window
    assert cm.should_hard_split_now() is False
    assert cm.should_soft_split_now() is False
    assert cm.next_split_trigger() is None                 # no auto-split slips through


def test_update_config_lower_threshold_arms_latch():
    """The symmetric case: lowering the threshold below the live reading on a live
    instance should ARM the latch (the user now wants splits to fire sooner)."""
    cm = ContextMonitor(SessionConfig(soft_context_percentage=85, hard_context_percentage=95,
                                      context_window_size=1_000_000))
    cm.ingest_context_usage(_usage(40, window=1_000_000))  # 40% -> below 85%, nothing armed
    assert cm.should_soft_split_now() is False
    cm.update_config(SessionConfig(soft_context_percentage=30, hard_context_percentage=95,
                                   context_window_size=1_000_000))
    assert cm.should_soft_split_now() is True              # 40% now exceeds the new 30% soft bar


def test_unsatisfiable_threshold_does_not_autosplit():
    """Regression: the infinite checkpoint/respawn loop. A threshold whose absolute
    token budget is below MIN_SPLIT_THRESHOLD_TOKENS is unsatisfiable (a fresh
    session immediately exceeds it), so auto-split must REFUSE rather than loop —
    while a manual split is still honored."""
    # hard=2% / soft=1% of a 200K window = 4,000 / 2,000 tokens, both << floor.
    cfg = SessionConfig(enabled=True, soft_context_percentage=1, hard_context_percentage=2,
                        context_window_size=200_000)
    cm = ContextMonitor(cfg)
    cm.ingest_context_usage(_usage(100))  # fresh session already pegged at 100%
    assert cm.should_hard_split_now() is False     # would loop -> refused
    assert cm.should_soft_split_now() is False
    assert cm.next_split_trigger() is None         # no auto-split fires

    cm.request_manual_split()                      # explicit human action bypasses the floor
    assert cm.next_split_trigger() == "manual"


def test_threshold_at_floor_boundary_still_fires():
    """A threshold whose budget meets the floor must still auto-split normally."""
    # 30% of 200K = 60,000 tokens, well above the 30,000-token floor.
    cm = ContextMonitor(SessionConfig(soft_context_percentage=30, hard_context_percentage=90,
                                      context_window_size=200_000))
    cm.ingest_context_usage(_usage(35))
    assert MIN_SPLIT_THRESHOLD_TOKENS == 30_000     # documents the contract
    assert cm.should_soft_split_now() is True
    assert cm.next_split_trigger() == "soft_context_35%"


def test_default_thresholds_are_85_and_95():
    """Defaults sit high on purpose: low split thresholds were the root cause of the
    checkpoint/respawn loop (a split fired on a session that had barely worked)."""
    assert DEFAULT_SOFT_CONTEXT_PERCENTAGE == 85
    assert DEFAULT_HARD_CONTEXT_PERCENTAGE == 95
    c = SessionConfig()
    assert c.effective_soft_pct() == 85 and c.effective_hard_pct() == 95


def test_ingest_adopts_model_resolved_window():
    """A model-aware window override (e.g. 1,000,000 for a [1m]/opus-4 model)
    supersedes the SessionConfig default everywhere: percentage math, the anti-loop
    floor, and the pressure readout all use it — not the stale 200K default. Without
    this, headroom on a 1M-context model was under-counted ~5x."""
    # config leaves window unset (0) -> monitor starts at the 200K default
    cm = ContextMonitor(SessionConfig(soft_context_percentage=85, hard_context_percentage=95))
    # 100K tokens is 50% of a 200K window but only 10% of a 1M window.
    usage = {"input_tokens": 100_000, "output_tokens": 0,
             "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
    cm.ingest_context_usage(usage, window_size=1_000_000)
    p = cm.pressure()
    assert p["context_window_size"] == 1_000_000   # adopted, not the 200K default
    assert p["used_percentage"] == 10.0            # 100K / 1M, not 50% of 200K
    assert cm.should_soft_split_now() is False     # 10% is well below the 85% soft threshold


def test_session_state_roundtrip():
    st = SessionState(current_session=3, last_used_pct=15.4,
                      last_total_tokens=154_000, last_window=1_000_000)
    st.checkpoints.append(__import__("agent_manager.session_manager", fromlist=["CheckpointMeta"]).CheckpointMeta(
        session=2, path="/x/checkpoint-2.md", trigger="manual", timestamp="2026-06-15T00:00:00Z"))
    rt = SessionState.from_dict(st.to_dict())
    assert rt.current_session == 3
    assert rt.checkpoints[0].session == 2 and rt.checkpoints[0].trigger == "manual"
    # The persisted pressure reading survives a save/load round-trip.
    assert rt.last_used_pct == 15.4
    assert rt.last_total_tokens == 154_000
    assert rt.last_window == 1_000_000


def test_seed_pressure_restores_display_without_arming_latches():
    """Restoring a persisted reading on restart must populate the pressure readout
    (so the UI bar is not stuck at 0% until the agent's first new turn) WITHOUT arming
    a split — a restart must never fire a split from a stale reading."""
    cm = ContextMonitor(SessionConfig(soft_context_percentage=85, hard_context_percentage=95,
                                       context_window_size=1_000_000))
    # Seed a reading that sits ABOVE the hard threshold (the worst case for safety).
    cm.seed_pressure(used_pct=96.0, total_tokens=960_000, window=1_000_000)
    p = cm.pressure()
    assert p["used_percentage"] == 96.0          # bar shows the restored value immediately
    assert p["total_context_tokens"] == 960_000
    assert p["context_window_size"] == 1_000_000
    # ...but no latch is armed, so nothing auto-splits off the stale reading.
    assert cm.should_hard_split_now() is False
    assert cm.next_split_trigger() is None
    # A genuine live reading still arms the split normally afterwards.
    cm.ingest_context_usage({"input_tokens": 960_000, "output_tokens": 0,
                             "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
                            window_size=1_000_000)
    assert cm.next_split_trigger() == "hard_context_96%"


def test_seed_pressure_ignores_empty_reading():
    """A zero/empty persisted reading (e.g. right after a post-split reset) must not
    seed anything — a fresh session legitimately starts at 0%."""
    cm = ContextMonitor(SessionConfig(context_window_size=1_000_000))
    cm.seed_pressure(used_pct=0.0, total_tokens=0, window=1_000_000)
    assert cm.pressure()["used_percentage"] == 0.0
    assert cm.pressure()["total_context_tokens"] == 0


if __name__ == "__main__":
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    passed = 0
    for fn in fns:
        fn()
        print(f"  PASS {fn.__name__}")
        passed += 1
    print(f"\n{passed}/{len(fns)} tests passed")
