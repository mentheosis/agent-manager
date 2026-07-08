from __future__ import annotations

import pytest
from fastapi.testclient import TestClient

from agent_manager.artifacts import artifact_id_for_path
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


def test_create_instance_accepts_memory_file(tmp_path) -> None:
    repo = tmp_path / "repo"
    memory = tmp_path / "memory.md"
    memory.write_text("project memory", encoding="utf-8")

    app = build_app()
    with TestClient(app) as c:
        r = c.post(
            "/api/instances",
            json={
                "name": "Memory Agent",
                "path": str(repo),
                "provider": "codex",
                "memory_file": str(memory),
            },
        )

        assert r.status_code == 201
        body = r.json()
        assert body["memory_file"] == str(memory)

        listed = c.get("/api/instances")
        assert listed.status_code == 200
        assert listed.json()[0]["memory_file"] == str(memory)


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


def test_provider_list_exposes_claude_and_codex() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.get("/api/providers")
        assert r.status_code == 200
        providers = {p["provider"]: p for p in r.json()}
        assert providers["claude"]["enabled"] is True
        assert providers["claude"]["auth"]["status_endpoint"] == "/api/providers/claude/auth/status"
        assert providers["codex"]["auth"]["status_endpoint"] == "/api/providers/codex/auth/status"
        assert providers["codex"]["runtime_options"]["default_permission_mode"] == "workspace-write"
        assert providers["codex"]["files"]["rules"] is True


def test_debug_session_by_title_and_session_id(tmp_path, monkeypatch) -> None:
    home = tmp_path / "home"
    monkeypatch.setenv("HOME", str(home))
    session_id = "019e567a-953f-7e41-9c62-cfd738101c85"
    session_file = home / ".codex" / "sessions" / "2026" / "05" / "23" / f"rollout-test-{session_id}.jsonl"
    session_file.parent.mkdir(parents=True)
    session_file.write_text('{"type":"thread.started"}\n{"type":"turn.completed"}\n', encoding="utf-8")

    app = build_app()
    with TestClient(app) as c:
        created = c.post(
            "/api/instances",
            json={"name": "Debug Codex", "path": str(tmp_path / "repo"), "provider": "codex"},
        )
        assert created.status_code == 201
        title = created.json()["title"]
        inst = app.state.registry.get(title)
        inst.session_id = session_id

        by_title = c.get(f"/api/debug/sessions/{title}?raw_tail=1")
        assert by_title.status_code == 200
        body = by_title.json()
        assert body["matched_by"] == "title"
        assert body["instance"]["title"] == title
        assert body["session_id"] == session_id
        assert body["debug"]["task"]["exists"] is True
        assert body["codex_session"]["found"] is True
        assert body["codex_session"]["line_count"] == 2
        assert body["codex_session"]["raw_tail"] == ['{"type":"turn.completed"}']

        by_session = c.get(f"/api/debug/sessions/{session_id}")
        assert by_session.status_code == 200
        assert by_session.json()["matched_by"] == "session_id"


def test_static_assets_are_not_cached() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.get("/style.css")
        assert r.status_code == 200
        assert r.headers["cache-control"] == "no-store"


def test_spa_routes_are_not_cached() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.get("/example/conversation")
        assert r.status_code == 200
        assert r.headers["cache-control"] == "no-store"


def test_image_artifact_route_serves_allowed_png(tmp_path) -> None:
    image = tmp_path / "shot.png"
    image.write_bytes(b"\x89PNG\r\n\x1a\n")

    app = build_app()
    with TestClient(app) as c:
        r = c.get(f"/api/artifacts/images/{artifact_id_for_path(image)}")
        assert r.status_code == 200
        assert r.content == b"\x89PNG\r\n\x1a\n"
        assert r.headers["content-type"] == "image/png"
        assert r.headers["cache-control"] == "no-store, max-age=0"
        assert r.headers["pragma"] == "no-cache"
        assert r.headers["expires"] == "0"
        assert "inline" in r.headers["content-disposition"]
        assert "shot-" in r.headers["content-disposition"]
        assert ".png" in r.headers["content-disposition"]


def test_instance_artifact_route_serves_workspace_gif(tmp_path) -> None:
    repo = tmp_path / "repo"
    repo.mkdir()
    image = repo / "animated.gif"
    image.write_bytes(b"GIF89a")

    app = build_app()
    with TestClient(app) as c:
        created = c.post("/api/instances", json={"name": "Artifacts", "path": str(repo)})
        assert created.status_code == 201
        title = created.json()["title"]

        r = c.get(f"/api/instances/{title}/artifacts/{artifact_id_for_path(image)}")
        assert r.status_code == 200
        assert r.content == b"GIF89a"
        assert r.headers["content-type"] == "image/gif"
        assert r.headers["cache-control"] == "no-store, max-age=0"


def test_instance_artifact_route_rejects_secret_file(tmp_path) -> None:
    repo = tmp_path / "repo"
    repo.mkdir()
    secret = repo / ".env"
    secret.write_text("TOKEN=secret\n", encoding="utf-8")

    app = build_app()
    with TestClient(app) as c:
        created = c.post("/api/instances", json={"name": "Artifacts", "path": str(repo)})
        assert created.status_code == 201
        title = created.json()["title"]

        r = c.get(f"/api/instances/{title}/artifacts/{artifact_id_for_path(secret)}")
        assert r.status_code == 403


def test_codex_instance_uses_codex_settings_files_and_default_mode(tmp_path, monkeypatch) -> None:
    home = tmp_path / "home"
    monkeypatch.setenv("HOME", str(home))

    app = build_app()
    with TestClient(app) as c:
        created = c.post(
            "/api/instances",
            json={"name": "Codex Project", "path": str(tmp_path / "repo"), "provider": "codex"},
        )
        assert created.status_code == 201
        body = created.json()
        assert body["provider"] == "codex"
        assert body["permission_mode"] == "workspace-write"

        rules = c.get(f"/api/instances/{body['title']}/rules")
        assert rules.status_code == 200
        assert [f["name"] for f in rules.json()["files"]] == [
            "AGENTS.md",
            "~/.codex/config.toml",
            ".mcp.json",
        ]

        codex_settings = next(f for f in rules.json()["files"] if f["name"] == "~/.codex/config.toml")
        saved = c.put(
            f"/api/instances/{body['title']}/rules",
            json={"path": codex_settings["path"], "content": 'model = "gpt-5"\n'},
        )
        assert saved.status_code == 200
        assert (home / ".codex" / "config.toml").read_text(encoding="utf-8") == 'model = "gpt-5"\n'


def test_update_permissions_can_clear_model_to_provider_default(tmp_path) -> None:
    app = build_app()
    with TestClient(app) as c:
        created = c.post(
            "/api/instances",
            json={"name": "Model Clear", "path": str(tmp_path / "repo"), "provider": "codex", "model": "gpt-5.5"},
        )
        assert created.status_code == 201
        title = created.json()["title"]
        assert created.json()["model"] == "gpt-5.5"

        cleared = c.patch(
            f"/api/instances/{title}/permissions",
            json={"permission_mode": "workspace-write", "model": None, "add_dirs": []},
        )
        assert cleared.status_code == 200
        assert cleared.json()["model"] is None


def test_provider_models_and_compat_models(monkeypatch) -> None:
    import agent_manager.server as server_mod

    async def fake_models(force_refresh: bool = False) -> list[str]:
        return ["claude-test-model"]

    async def fake_codex_models() -> list[str]:
        return ["gpt-5.5"]

    monkeypatch.setattr(server_mod, "_fetch_models", fake_models)
    monkeypatch.setattr(server_mod, "fetch_codex_models", fake_codex_models)

    app = build_app()
    with TestClient(app) as c:
        assert c.get("/api/models").json() == ["claude-test-model"]
        assert c.get("/api/models?provider=claude").json() == ["claude-test-model"]
        assert c.get("/api/providers/claude/models").json() == ["claude-test-model"]
        assert c.get("/api/providers/codex/models").json() == ["gpt-5.5"]
        assert c.get("/api/providers/nope/models").status_code == 404


def test_claude_models_return_fallback_without_api_key(monkeypatch) -> None:
    import agent_manager.server as server_mod

    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.setattr(server_mod, "_models_cache", None)
    # CLI binary extraction is environment-dependent (present in the docker
    # image, absent on dev machines) — stub it out to exercise the fallback.
    monkeypatch.setattr(server_mod, "_extract_models_from_cli", lambda: [])

    app = build_app()
    with TestClient(app) as c:
        assert c.get("/api/providers/claude/models").json() == server_mod._FALLBACK_MODELS


def test_provider_auth_status_and_compat_alias() -> None:
    app = build_app()
    with TestClient(app) as c:
        compat = c.get("/api/auth/status")
        provider = c.get("/api/providers/claude/auth/status")
        codex = c.get("/api/providers/codex/auth/status")

        assert compat.status_code == 200
        assert provider.status_code == 200
        assert codex.status_code == 200
        assert provider.json()["provider"] == "claude"
        assert provider.json()["authed"] == compat.json()["authed"]
        assert codex.json()["provider"] == "codex"
        assert "login_supported" in codex.json()


def test_provider_login_returns_404_for_unknown_provider() -> None:
    app = build_app()
    with TestClient(app) as c:
        r = c.post("/api/providers/nope/auth/login")
        assert r.status_code == 404


def test_claude_login_command_uses_auth_subcommand() -> None:
    from agent_manager.auth import LOGIN_COMMAND

    assert LOGIN_COMMAND == ("claude", "auth", "login")


def test_codex_login_command_uses_default_browser_flow() -> None:
    from agent_manager.auth import CODEX_LOGIN_COMMAND

    assert CODEX_LOGIN_COMMAND == ("codex", "login")


def test_parse_claude_auth_status() -> None:
    from agent_manager.auth import _parse_auth_status

    assert _parse_auth_status('{"loggedIn": true, "authMethod": "claudeai"}') == {
        "loggedIn": True,
        "authMethod": "claudeai",
    }
    assert _parse_auth_status("not json") == {}


def test_auth_status_treats_existing_credentials_as_authenticated(tmp_path) -> None:
    from agent_manager.auth import _status_payload

    credentials = tmp_path / ".credentials.json"
    credentials.write_text("{}", encoding="utf-8")

    payload = _status_payload(
        provider="claude",
        status={"loggedIn": False},
        credentials_path=credentials,
        login_supported=True,
    )

    assert payload["authed"] is True
    assert payload["credentials_present"] is True


def test_classify_runtime_auth_failure() -> None:
    from agent_manager.auth import classify_runtime_auth_failure

    assert classify_runtime_auth_failure("Error: 401 Unauthorized") == "expired"
    assert classify_runtime_auth_failure("CLIConnectionError: OAuth token has expired") == "expired"
    assert classify_runtime_auth_failure("ProcessError: exit 1: 403 Forbidden") == "expired"
    assert classify_runtime_auth_failure("HTTPStatusError: 502 Bad Gateway") == "gateway"
    assert classify_runtime_auth_failure("ProcessError: 503 Service Unavailable") == "gateway"
    assert classify_runtime_auth_failure("ValueError: unrelated") is None
    assert classify_runtime_auth_failure("") is None
    assert classify_runtime_auth_failure(None) is None


def test_auth_registry_needs_reauth_flag_in_status() -> None:
    from agent_manager.auth import AuthRegistry

    r = AuthRegistry()
    assert r.provider_status()["needs_reauth"] is False
    assert r.provider_status()["reauth_reason"] is None

    r.mark_needs_reauth("expired", "CLIConnectionError: 401")
    status = r.provider_status()
    assert status["needs_reauth"] is True
    assert status["reauth_reason"] == "expired"

    r.clear_needs_reauth()
    assert r.provider_status()["needs_reauth"] is False
    assert r.provider_status()["reauth_reason"] is None


def test_provider_auth_status_exposes_reauth_fields() -> None:
    app = build_app()
    with TestClient(app) as c:
        body = c.get("/api/providers/claude/auth/status").json()
        assert "needs_reauth" in body
        assert "reauth_reason" in body


def test_instance_record_migrates_legacy_claude_agent() -> None:
    from agent_manager.persistence import InstanceRecord

    rec = InstanceRecord.from_dict({
        "title": "old",
        "path": "/tmp/old",
        "instance_type": "claude",
    })

    assert rec.provider == "claude"
    assert rec.kind == "agent"
    assert rec.instance_type == "claude"
    assert rec.to_dict()["provider"] == "claude"
    assert rec.to_dict()["kind"] == "agent"


def test_instance_record_migrates_legacy_loop() -> None:
    from agent_manager.persistence import InstanceRecord

    rec = InstanceRecord.from_dict({
        "title": "team",
        "path": "/tmp/team",
        "instance_type": "loop",
    })

    assert rec.provider == "claude"
    assert rec.kind == "loop"
    assert rec.instance_type == "loop"


def test_instance_record_preserves_new_provider_kind_fields() -> None:
    from agent_manager.persistence import InstanceRecord

    rec = InstanceRecord.from_dict({
        "title": "future",
        "path": "/tmp/future",
        "provider": "codex",
        "kind": "agent",
        "instance_type": "codex",
    })

    assert rec.provider == "codex"
    assert rec.kind == "agent"
    assert rec.instance_type == "codex"


def test_slugify_basic_cases() -> None:
    from agent_manager.state import slugify

    assert slugify("My Cool Project") == "my_cool_project"
    assert slugify("  spaced  ") == "spaced"
    assert slugify("a/b\\c?d") == "abcd"
    assert slugify("---hello---") == "hello"
    assert slugify("") == "instance"
    assert slugify("   ") == "instance"
    assert slugify("ñoño") == "oo"  # non-ascii chars are dropped
    assert slugify("Has___underscores") == "has_underscores"
    assert slugify("a" * 200) == "a" * 64


def test_model_regex_extracts_clean_ids_and_rejects_dated_variants() -> None:
    """Simulates strings-scanning a binary that contains a mix of ID forms."""
    from agent_manager.server import _CLI_MODEL_RE

    binary_blob = (
        b"noise\x00claude-opus-4\x01claude-opus-4-8\x00"
        b"claude-opus-4-6[1m]\x00claude-opus-4-20250514\x00"
        b"claude-opus-4-1@20250805\x00claude-sonnet-4-5-20250929[1m]\x00"
        b"claude-haiku-3-5\x00claude-fable-5\x00random-string-here"
    )
    matches = sorted({m.decode() for m in _CLI_MODEL_RE.findall(binary_blob)})
    # Dated variants (`-20250514`) and `@` aliases (`@20250805`) are rejected.
    # `claude-sonnet-4-5-20250929[1m]` is also dated → rejected.
    assert matches == sorted([
        "claude-opus-4",
        "claude-opus-4-8",
        "claude-opus-4-6[1m]",
        "claude-haiku-3-5",
        "claude-fable-5",
    ])


def test_model_sort_key_groups_families_and_orders_versions() -> None:
    from agent_manager.server import _model_sort_key

    unsorted = [
        "claude-sonnet-4-5",
        "claude-opus-4-8",
        "claude-fable-5",
        "claude-opus-4-6[1m]",
        "claude-opus-4-6",
        "claude-haiku-3-5",
    ]
    ordered = sorted(unsorted, key=_model_sort_key)
    assert ordered == [
        # opus family first, newer versions before older, base before [1m]
        "claude-opus-4-8",
        "claude-opus-4-6",
        "claude-opus-4-6[1m]",
        # then sonnet
        "claude-sonnet-4-5",
        # then haiku
        "claude-haiku-3-5",
        # then fable
        "claude-fable-5",
    ]
