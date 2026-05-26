from __future__ import annotations

import asyncio
import json
import logging
import math
import time
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)

_MODELS_CACHE_TTL_SECONDS = 300
_DOCTOR_CACHE_TTL_SECONDS = 60

_models_cache: tuple[float, list[str]] | None = None
_doctor_cache: tuple[float, dict[str, Any]] | None = None


async def fetch_codex_models(timeout: float = 10) -> list[str]:
    """Return Codex model slugs available to the current CLI auth/config."""
    global _models_cache
    now = time.monotonic()
    if _models_cache and now - _models_cache[0] < _MODELS_CACHE_TTL_SECONDS:
        return list(_models_cache[1])

    data = await _run_json(["codex", "debug", "models"], timeout=timeout)
    models = _parse_codex_model_catalog(data)
    if models:
        _models_cache = (now, models)
        log.info("fetched %d Codex model(s) from CLI catalog", len(models))
    return models


async def fetch_codex_runtime_metadata(cwd: str | None = None, timeout: float = 3) -> dict[str, Any]:
    """Return non-secret Codex CLI metadata suitable for system_init events."""
    global _doctor_cache
    now = time.monotonic()
    if _doctor_cache and now - _doctor_cache[0] < _DOCTOR_CACHE_TTL_SECONDS:
        return dict(_doctor_cache[1])

    data = await _run_json(["codex", "doctor", "--json"], cwd=cwd, timeout=timeout)
    metadata = _parse_codex_doctor_metadata(data)
    if metadata:
        _doctor_cache = (now, metadata)
    return metadata


def _parse_codex_model_catalog(data: dict[str, Any]) -> list[str]:
    models = data.get("models")
    if not isinstance(models, list):
        return []

    ranked: list[tuple[float, int, str]] = []
    for idx, item in enumerate(models):
        if not isinstance(item, dict):
            continue
        if item.get("visibility") != "list":
            continue
        if item.get("upgrade"):
            continue
        slug = item.get("slug")
        if not isinstance(slug, str) or not slug:
            continue
        priority = item.get("priority")
        rank = float(priority) if isinstance(priority, (int, float)) and math.isfinite(priority) else float(idx)
        ranked.append((rank, idx, slug))

    seen: set[str] = set()
    result: list[str] = []
    for _, _, slug in sorted(ranked):
        if slug not in seen:
            seen.add(slug)
            result.append(slug)
    return result


def _parse_codex_doctor_metadata(data: dict[str, Any]) -> dict[str, Any]:
    checks = data.get("checks")
    checks = checks if isinstance(checks, dict) else {}

    config = _check_details(checks, "config.load")
    auth = _check_details(checks, "auth.credentials")
    runtime = _check_details(checks, "runtime.provenance")

    metadata: dict[str, Any] = {}
    codex_version = data.get("codexVersion") or runtime.get("version")
    if isinstance(codex_version, str) and codex_version:
        metadata["cli_version"] = codex_version

    model = config.get("model")
    if isinstance(model, str) and model:
        metadata["configured_model"] = model
        metadata["active_model_label"] = model

    model_provider = config.get("model provider")
    if isinstance(model_provider, str) and model_provider:
        metadata["model_provider"] = model_provider

    auth_mode = auth.get("stored auth mode")
    if isinstance(auth_mode, str) and auth_mode:
        metadata["auth_mode"] = auth_mode

    return metadata


async def _run_json(cmd: list[str], cwd: str | None = None, timeout: float = 10) -> dict[str, Any]:
    proc: asyncio.subprocess.Process | None = None
    try:
        proc = await asyncio.create_subprocess_exec(
            *cmd,
            cwd=_safe_cwd(cwd),
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=timeout)
    except FileNotFoundError:
        log.info("codex CLI is not installed")
        return {}
    except asyncio.TimeoutError:
        if proc is not None and proc.returncode is None:
            proc.kill()
            await proc.wait()
        log.warning("%s timed out after %.1fs", cmd[:3], timeout)
        return {}
    except OSError:
        log.warning("failed to run %s", cmd[:3], exc_info=True)
        return {}

    text = stdout.decode("utf-8", errors="replace").strip()
    if not text:
        if proc.returncode not in (0, None):
            err = stderr.decode("utf-8", errors="replace").strip()
            log.info("%s exited with %s: %s", cmd[:3], proc.returncode, err[:400])
        return {}
    try:
        data = json.loads(text)
    except json.JSONDecodeError:
        log.warning("%s returned non-JSON output: %s", cmd[:3], text[:400])
        return {}
    return data if isinstance(data, dict) else {}


def _check_details(checks: dict[str, Any], key: str) -> dict[str, Any]:
    check = checks.get(key)
    if not isinstance(check, dict):
        return {}
    details = check.get("details")
    return details if isinstance(details, dict) else {}


def _safe_cwd(cwd: str | None) -> str | None:
    if not cwd:
        return None
    return cwd if Path(cwd).is_dir() else None
