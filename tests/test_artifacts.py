from __future__ import annotations

from agent_manager.artifacts import artifact_path_is_present, extract_artifact_directives


def test_extract_artifact_directive_for_animated_gif() -> None:
    cleaned, artifacts = extract_artifact_directives(
        'See this [[agent-manager:artifact path="/tmp/demo.gif" title="Animated preview"]].',
        source="codex",
    )

    assert cleaned == "See this ."
    assert artifacts == [
        {
            "type": "artifact",
            "artifact_type": "image",
            "artifact_id": "L3RtcC9kZW1vLmdpZg",
            "path": "/tmp/demo.gif",
            "title": "Animated preview",
            "mime_type": "image/gif",
            "source": "codex",
        }
    ]


def test_extract_artifact_directive_for_generic_file() -> None:
    cleaned, artifacts = extract_artifact_directives(
        'Report: [[agent-manager:artifact path="/tmp/report.json" title="JSON report" type="file"]]',
        source="claude",
    )

    assert cleaned == "Report:"
    assert artifacts[0]["artifact_type"] == "file"
    assert artifacts[0]["mime_type"] == "application/json"


def test_placeholder_artifact_path_is_not_present() -> None:
    assert artifact_path_is_present("/real/absolute/path/to/file.png") is False
