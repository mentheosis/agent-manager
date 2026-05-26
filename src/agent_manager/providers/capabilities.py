from __future__ import annotations

import shutil
from typing import Any


CLAUDE_PERMISSION_MODES = ["default", "acceptEdits", "plan", "bypassPermissions"]


def provider_capabilities(provider: str) -> dict[str, Any]:
    if provider == "claude":
        return {
            "provider": "claude",
            "label": "Claude Code",
            "enabled": True,
            "available": shutil.which("claude") is not None,
            "models": {"endpoint": "/api/providers/claude/models", "supports_custom": True},
            "auth": {
                "mode": "pty-login",
                "status_endpoint": "/api/providers/claude/auth/status",
                "login_endpoint": "/api/providers/claude/auth/login",
                "login_supported": True,
            },
            "runtime_options": {
                "permission_modes": CLAUDE_PERMISSION_MODES,
                "default_permission_mode": "acceptEdits",
                "add_dirs": True,
                "images": True,
            },
            "files": {
                "rules": True,
                "plans": True,
                "memory": True,
            },
        }
    if provider == "codex":
        available = shutil.which("codex") is not None
        return {
            "provider": "codex",
            "label": "Codex",
            "enabled": available,
            "available": available,
            "models": {"endpoint": "/api/providers/codex/models", "supports_custom": False},
            "auth": {
                "mode": "device-auth",
                "status_endpoint": "/api/providers/codex/auth/status",
                "login_endpoint": "/api/providers/codex/auth/login",
                "login_supported": available,
            },
            "runtime_options": {
                "permission_modes": ["workspace-write", "read-only", "danger-full-access"],
                "default_permission_mode": "workspace-write",
                "add_dirs": True,
                "images": True,
            },
            "files": {
                "rules": True,
                "plans": False,
                "memory": False,
            },
        }
    raise KeyError(provider)


def list_provider_capabilities() -> list[dict[str, Any]]:
    return [provider_capabilities("claude"), provider_capabilities("codex")]
