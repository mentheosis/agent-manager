"""Tests for the memory-file freshness tracker."""
from __future__ import annotations

from pathlib import Path

from agent_manager.providers.base import (
    MemoryFileTracker,
    format_memory_block,
    memory_file_hash,
    read_memory_file,
)


def test_memory_file_hash_returns_none_when_unset() -> None:
    assert memory_file_hash(None) is None
    assert memory_file_hash("") is None


def test_memory_file_hash_returns_none_for_missing_file(tmp_path: Path) -> None:
    assert memory_file_hash(str(tmp_path / "does_not_exist.md")) is None


def test_memory_file_hash_returns_none_for_empty_file(tmp_path: Path) -> None:
    p = tmp_path / "empty.md"
    p.write_text("")
    assert memory_file_hash(str(p)) is None


def test_memory_file_hash_is_stable_across_calls(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("Hello world.")
    h1 = memory_file_hash(str(p))
    h2 = memory_file_hash(str(p))
    assert h1 is not None
    assert h1 == h2


def test_memory_file_hash_changes_with_content(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("Hello.")
    h1 = memory_file_hash(str(p))
    p.write_text("Hello world.")
    h2 = memory_file_hash(str(p))
    assert h1 != h2


def test_tracker_initial_state_records_hash(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("v1")
    tracker = MemoryFileTracker(str(p))
    assert tracker.current_hash is not None
    assert tracker.has_changed() is False


def test_tracker_detects_edit_without_updating(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("v1")
    tracker = MemoryFileTracker(str(p))

    p.write_text("v2")
    # has_changed() should detect but NOT update
    assert tracker.has_changed() is True
    assert tracker.has_changed() is True  # still true, stored hash unchanged


def test_tracker_refresh_updates_and_returns_change_signal(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("v1")
    tracker = MemoryFileTracker(str(p))
    h0 = tracker.current_hash

    # No change: refresh returns False
    assert tracker.refresh() is False
    assert tracker.current_hash == h0

    # Edit: refresh returns True and updates the hash
    p.write_text("v2")
    assert tracker.refresh() is True
    assert tracker.current_hash != h0
    # Idempotent: a second refresh returns False
    assert tracker.refresh() is False


def test_tracker_handles_missing_file(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("v1")
    tracker = MemoryFileTracker(str(p))

    p.unlink()
    # File disappearing counts as a change
    assert tracker.refresh() is True
    assert tracker.current_hash is None
    # And reappearing also counts
    p.write_text("v2")
    assert tracker.refresh() is True
    assert tracker.current_hash is not None


def test_tracker_with_no_memory_file_is_stable() -> None:
    tracker = MemoryFileTracker(None)
    assert tracker.current_hash is None
    assert tracker.refresh() is False
    assert tracker.has_changed() is False


def test_format_memory_block_includes_path_attribute(tmp_path: Path) -> None:
    """The <memory> block should include the resolved path as an attribute."""
    p = tmp_path / "memory.md"
    p.write_text("Project context here.")
    block = format_memory_block(str(p))
    assert block is not None
    assert f'source="{p.resolve()}"' in block
    assert "Project context here." in block
    assert "<memory" in block
    assert "</memory>" in block


def test_read_memory_file_strips_surrounding_whitespace(tmp_path: Path) -> None:
    p = tmp_path / "memory.md"
    p.write_text("\n\n  content  \n\n")
    assert read_memory_file(str(p)) == "content"
