from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from agent_manager.server import build_app


@pytest.fixture(autouse=True)
def _state_dir(tmp_path, monkeypatch):
    """Redirect persistence to a temp dir for every test."""
    monkeypatch.setenv("AGENT_MANAGER_STATE_DIR", str(tmp_path))


def test_list_empty() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.get("/api/instances")
        assert r.status_code == 200
        assert r.json() == []


def test_get_missing_returns_404() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.get("/api/instances/nonexistent")
        assert r.status_code == 404


def test_send_to_missing_returns_404() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.post("/api/instances/nope/send", json={"text": "hi"})
        assert r.status_code == 404


def test_create_validation() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.post("/api/instances", json={"name": "", "path": "/tmp"})
        assert r.status_code == 422


def test_rename_missing_returns_404() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.patch("/api/instances/nope/rename", json={"display_title": "Foo"})
        assert r.status_code == 404


def test_reorder_mismatched_titles_returns_400() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.post("/api/instances/reorder", json={"titles": ["a", "b"]})
        assert r.status_code == 400


def test_auth_status_returns_authed_field() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.get("/api/auth/status")
        assert r.status_code == 200
        body = r.json()
        assert "authed" in body
        assert isinstance(body["authed"], bool)


def test_slugify_basic_cases() -> None:
    from agent_manager.state import slugify

    assert slugify("My Cool Project") == "my_cool_project"
    assert slugify("  spaced  ") == "spaced"
    assert slugify("a/b\\c?d") == "abcd"
    assert slugify("---hello---") == "hello"
    assert slugify("") == "instance"
    assert slugify("   ") == "instance"
    assert slugify("ñoño") == "instance"  # ascii-only
    assert slugify("Has___underscores") == "has_underscores"
    assert slugify("a" * 200) == "a" * 64
