#!/usr/bin/env python3
"""Import optional host credentials and CA trust before starting the server."""
import hashlib
import json
import os
from pathlib import Path
import ssl
import sys
import tempfile


def atomic_write(path: Path, data: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, name = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
        os.replace(name, path)
    finally:
        Path(name).unlink(missing_ok=True)


def configure(env: dict[str, str]) -> None:
    source = env.get("AGENT_MANAGER_CODEX_AUTH_SOURCE")
    if source:
        data = Path(source).read_bytes()
        auth = json.loads(data)
        if not isinstance(auth, dict) or not (auth.get("tokens") or auth.get("OPENAI_API_KEY")):
            raise ValueError("Host Codex auth file contains no credentials")
        home = Path(env.get("CODEX_HOME") or str(Path(env["HOME"]) / ".codex"))
        target = home / "auth.json"
        marker = home / ".host-auth.sha256"
        digest = hashlib.sha256(data).hexdigest().encode()
        # Keep tokens refreshed inside the container when the host file has
        # not changed. Never write credentials back to the host mount.
        if not target.exists() or not marker.exists() or marker.read_bytes() != digest:
            atomic_write(target, data)
            atomic_write(marker, digest)
            print("Imported host Codex credentials", flush=True)

    source = env.get("AGENT_MANAGER_EXTRA_CA_CERTS")
    if source:
        extra = Path(source).read_bytes()
        # Fail startup for invalid CA input instead of silently losing trust.
        ssl.create_default_context(cadata=extra.decode("ascii"))
        system = Path(env.get("SSL_CERT_FILE") or "/etc/ssl/certs/ca-certificates.crt")
        bundle = Path(env["HOME"]) / ".local/share/agent-manager/ca-bundle.pem"
        atomic_write(bundle, system.read_bytes() + b"\n" + extra)
        for name in ("SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS"):
            env[name] = str(bundle)
        print("Configured additional host CA trust", flush=True)


if __name__ == "__main__":
    configure(os.environ)
    command = sys.argv[1:] or ["agent-manager"]
    os.execvp(command[0], command)
