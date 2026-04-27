from __future__ import annotations

import logging
import os

import uvicorn


def main() -> None:
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    host = os.environ.get("AGENT_MANAGER_HOST", "127.0.0.1")
    port = int(os.environ.get("AGENT_MANAGER_PORT", "8787"))
    uvicorn.run(
        "agent_manager.server:build_app",
        factory=True,
        host=host,
        port=port,
        log_level="info",
    )


if __name__ == "__main__":
    main()
