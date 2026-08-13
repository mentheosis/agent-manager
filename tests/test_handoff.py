"""Tests for checkpoint.py + handoff.py (the SDK session-management port).

Runnable two ways:
  * pytest:     pytest tests/test_handoff.py
  * standalone: PYTHONPATH=src python3 tests/test_handoff.py

Async tests are wrapped with asyncio.run so no pytest-asyncio plugin is needed.
"""

from __future__ import annotations

import asyncio
import os
import tempfile

# Point the app state dir at a throwaway location BEFORE importing the modules
# that read it (checkpoint._sessions_root reads it per-call, but set early anyway).
_TMP = tempfile.mkdtemp(prefix="agentmgr-test-")
os.environ["AGENT_MANAGER_STATE_DIR"] = _TMP

from agent_manager import checkpoint  # noqa: E402
from agent_manager.handoff import HandoffCallbacks, HandoffCoordinator, HandoffPhase  # noqa: E402
from agent_manager.session_manager import SessionConfig, SessionState  # noqa: E402


# --- checkpoint.py -------------------------------------------------------

def test_checkpoint_path_shape():
    p = checkpoint.checkpoint_path("agentA", 3)
    assert p.name == "checkpoint-3.md"
    assert p.parent.name == "agentA"
    assert p.parent.parent.name == "sessions"


def test_unsafe_title_rejected():
    for bad in ["../evil", "a/b", "a b", ""]:
        try:
            checkpoint.session_dir(bad)
        except ValueError:
            continue
        raise AssertionError(f"expected ValueError for {bad!r}")


def test_mtime_absent_then_present():
    title = "ckpt_mtime"
    checkpoint.ensure_session_dir(title)
    assert checkpoint.checkpoint_mtime_ns(title, 1) == (0, False)
    assert checkpoint.checkpoint_exists(title, 1) is False

    checkpoint.checkpoint_path(title, 1).write_text("## CURRENT STATE\nx", encoding="utf-8")
    mtime_ns, exists = checkpoint.checkpoint_mtime_ns(title, 1)
    assert exists is True and mtime_ns > 0
    assert "## CURRENT STATE" in checkpoint.read_checkpoint(title, 1)


def test_prompts_contain_path_and_sections():
    title = "ckpt_prompts"
    path = str(checkpoint.checkpoint_path(title, 2))
    wrap = checkpoint.wrap_up_prompt(title, 2)
    hard = checkpoint.hard_wrap_up_prompt(title, 2)
    restore = checkpoint.restore_prompt(title, 2)

    assert path in wrap and "## CURRENT STATE" in wrap and "session boundary" in wrap
    assert path in hard and "ending NOW" in hard and "## NEXT ACTION" in hard
    assert path in restore and "continuing a workflow" in restore
    # The restore prompt must NOT read like a wrap-up (the test fakes rely on this).
    assert "continuing a workflow" not in wrap


def test_cleanup_session_dir():
    title = "ckpt_cleanup"
    checkpoint.ensure_session_dir(title)
    checkpoint.checkpoint_path(title, 1).write_text("x", encoding="utf-8")
    assert checkpoint.session_dir(title).exists()
    checkpoint.cleanup_session_dir(title)
    assert not checkpoint.session_dir(title).exists()


# --- handoff.py ----------------------------------------------------------

def _coordinator(title, *, writes_checkpoint: bool, status="ready"):
    """Build a coordinator with fake callbacks. The fake send_prompt simulates the
    agent writing its checkpoint on the wrap-up prompt (when writes_checkpoint)."""
    state = SessionState(current_session=1)
    cfg = SessionConfig(enabled=True, checkpoint_timeout_sec=1)  # 1s/attempt, polled fast below
    sent: list[str] = []
    calls = {"respawn": 0, "saved": 0}
    start_session = state.current_session

    async def send_prompt(text: str) -> None:
        sent.append(text)
        # Wrap-up prompts (not the restore prompt) trigger a checkpoint write.
        if writes_checkpoint and "continuing a workflow" not in text:
            checkpoint.ensure_session_dir(title)
            checkpoint.checkpoint_path(title, start_session).write_text(
                "## CURRENT STATE\ndone", encoding="utf-8"
            )

    async def respawn_fresh() -> None:
        calls["respawn"] += 1

    async def on_save(_state) -> None:
        calls["saved"] += 1

    cb = HandoffCallbacks(
        send_prompt=send_prompt,
        respawn_fresh=respawn_fresh,
        get_status=lambda: status,
        on_save=on_save,
    )
    coord = HandoffCoordinator(
        title, cfg, state, cb,
        poll_interval_sec=0.01, ready_timeout_sec=0.05,
    )
    return coord, state, sent, calls


def test_full_cycle_advances_and_respawns():
    async def run():
        title = "hf_full"
        coord, state, sent, calls = _coordinator(title, writes_checkpoint=True)
        await coord.trigger_split("manual")
        await coord.wait()

        assert calls["respawn"] == 1, "should respawn exactly once"
        assert state.current_session == 2, "session number should advance 1 -> 2"
        assert len(state.checkpoints) == 1 and state.checkpoints[0].trigger == "manual"
        assert any("continuing a workflow" in s for s in sent), "restore prompt must be sent"
        assert calls["saved"] == 1
        assert coord.status().phase == HandoffPhase.IDLE
        assert coord.status().error is None
    asyncio.run(run())


def test_no_respawn_without_checkpoint():
    async def run():
        title = "hf_nockpt"
        coord, state, sent, calls = _coordinator(title, writes_checkpoint=False)
        await coord.trigger_split("manual")
        await coord.wait()

        assert calls["respawn"] == 0, "must NOT respawn when no checkpoint was written"
        assert state.current_session == 1, "session number must not advance"
        assert coord.status().error is not None
    asyncio.run(run())


def test_stale_checkpoint_ignored():
    async def run():
        title = "hf_stale"
        # Pre-create a checkpoint with a recent mtime; the agent (fake) does NOT rewrite it.
        checkpoint.ensure_session_dir(title)
        checkpoint.checkpoint_path(title, 1).write_text("## CURRENT STATE\nold", encoding="utf-8")
        coord, state, sent, calls = _coordinator(title, writes_checkpoint=False)
        await coord.trigger_split("manual")
        await coord.wait()

        assert calls["respawn"] == 0, "a stale (unchanged) checkpoint must not trigger a respawn"
        assert state.current_session == 1
        assert coord.status().error is not None
    asyncio.run(run())


def test_fresh_checkpoint_over_stale():
    async def run():
        title = "hf_fresh"
        # Pre-create a stale checkpoint, backdate its mtime by an hour.
        checkpoint.ensure_session_dir(title)
        p = checkpoint.checkpoint_path(title, 1)
        p.write_text("## CURRENT STATE\nstale", encoding="utf-8")
        old = p.stat().st_mtime - 3600
        os.utime(p, (old, old))
        # The agent rewrites it during this handoff → newer mtime → accepted.
        coord, state, sent, calls = _coordinator(title, writes_checkpoint=True)
        await coord.trigger_split("manual")
        await coord.wait()

        assert calls["respawn"] == 1, "a freshly-rewritten checkpoint must be accepted over a stale one"
        assert state.current_session == 2
    asyncio.run(run())


def test_hard_vs_soft_wrapup_prompt():
    async def run():
        # hard trigger -> urgent prompt
        coord, _state, sent_hard, _ = _coordinator("hf_hard", writes_checkpoint=True)
        await coord.trigger_split("hard_context_92%")
        await coord.wait()
        assert "ending NOW" in sent_hard[0], "hard split should use the urgent wrap-up prompt"

        # manual trigger -> soft prompt
        coord2, _s2, sent_soft, _ = _coordinator("hf_soft", writes_checkpoint=True)
        await coord2.trigger_split("manual")
        await coord2.wait()
        assert "session boundary" in sent_soft[0], "soft/manual split should use the standard wrap-up prompt"
    asyncio.run(run())


def test_persistence_record_session_fields_roundtrip():
    from agent_manager.persistence import InstanceRecord

    rec = InstanceRecord(
        title="x", path="/p",
        session_config={"enabled": True, "soft_context_percentage": 65},
        session_state={"current_session": 4, "checkpoints": []},
    )
    rt = InstanceRecord.from_dict(rec.to_dict())
    assert rt.session_config == {"enabled": True, "soft_context_percentage": 65}
    assert rt.session_state == {"current_session": 4, "checkpoints": []}

    # Backward compatibility: a legacy record with no session fields defaults to None.
    legacy = InstanceRecord.from_dict({"title": "y", "path": "/q"})
    assert legacy.session_config is None and legacy.session_state is None


if __name__ == "__main__":
    fns = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    passed = 0
    for fn in fns:
        fn()
        print(f"  PASS {fn.__name__}")
        passed += 1
    print(f"\n{passed}/{len(fns)} tests passed")
