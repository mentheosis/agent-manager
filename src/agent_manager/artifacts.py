from __future__ import annotations

import base64
import mimetypes
import re
import shlex
from pathlib import Path
from typing import Any


IMAGE_SUFFIXES = {".png", ".jpg", ".jpeg", ".webp", ".gif"}
VIDEO_SUFFIXES = {".mp4", ".webm", ".ogv", ".ogg", ".mov", ".m4v"}
ARTIFACT_DIRECTIVE_RE = re.compile(r"\[\[agent-manager:artifact\s+(?P<attrs>[^\]]+)\]\]")


def artifact_id_for_path(path: str | Path) -> str:
    raw = str(Path(path))
    return base64.urlsafe_b64encode(raw.encode("utf-8")).decode("ascii").rstrip("=")


def path_for_artifact_id(artifact_id: str) -> Path:
    padded = artifact_id + "=" * (-len(artifact_id) % 4)
    raw = base64.urlsafe_b64decode(padded.encode("ascii")).decode("utf-8")
    return Path(raw)


def artifact_event(
    path: str | Path,
    *,
    title: str | None = None,
    source: str | None = None,
    artifact_type: str | None = None,
) -> dict[str, Any]:
    artifact_path = Path(path)
    inferred_type = artifact_type or _artifact_type_for_path(artifact_path)
    event = {
        "type": "artifact",
        "artifact_type": inferred_type,
        "artifact_id": artifact_id_for_path(artifact_path),
        "path": str(artifact_path),
        "title": title or artifact_path.name,
        "mime_type": _mime_type_for_path(artifact_path),
    }
    if source:
        event["source"] = source
    return event


def image_artifact_event(path: str | Path, *, title: str | None = None, source: str | None = None) -> dict[str, Any]:
    return artifact_event(path, title=title, source=source, artifact_type="image")


def is_image_path(path: str | Path) -> bool:
    return Path(path).suffix.lower() in IMAGE_SUFFIXES


def is_video_path(path: str | Path) -> bool:
    return Path(path).suffix.lower() in VIDEO_SUFFIXES


def _artifact_type_for_path(path: str | Path) -> str:
    if is_image_path(path):
        return "image"
    if is_video_path(path):
        return "video"
    return "file"


def extract_artifact_directives(text: str, *, source: str | None = None) -> tuple[str, list[dict[str, Any]]]:
    artifacts: list[dict[str, Any]] = []

    def replace(match: re.Match[str]) -> str:
        attrs = _parse_attrs(match.group("attrs"))
        path = attrs.get("path")
        if not path:
            return match.group(0)
        artifact_type = attrs.get("type")
        artifacts.append(
            artifact_event(
                path,
                title=attrs.get("title"),
                source=source,
                artifact_type=artifact_type,
            )
        )
        return ""

    cleaned = ARTIFACT_DIRECTIVE_RE.sub(replace, text)
    cleaned = re.sub(r"\n{3,}", "\n\n", cleaned).strip()
    return cleaned, artifacts


def artifact_instruction() -> str:
    return (
        " Note: if you want to display a file to the user in our custom UI, such as an existing image, you can emit this syntax filling in the actual filepath: "
        '[[agent-manager:artifact path="/real/absolute/path/to/file.png" title="Short title"]]. '
    )


def artifact_path_is_present(path: str | Path) -> bool:
    try:
        candidate = Path(path).expanduser()
        return candidate.is_absolute() and candidate.is_file()
    except OSError:
        return False


def _parse_attrs(raw: str) -> dict[str, str]:
    try:
        parts = shlex.split(raw)
    except ValueError:
        return {}
    attrs: dict[str, str] = {}
    for part in parts:
        if "=" not in part:
            continue
        key, value = part.split("=", 1)
        key = key.strip().lower()
        if key in {"path", "title", "type"}:
            attrs[key] = value
    return attrs


def _mime_type_for_path(path: Path) -> str:
    guessed = mimetypes.guess_type(path.name)[0]
    if guessed:
        return guessed
    if is_image_path(path):
        return "image/png"
    if is_video_path(path):
        return "video/mp4"
    return "application/octet-stream"
