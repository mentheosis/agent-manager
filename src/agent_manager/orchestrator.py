"""
Orchestrator process management.

Spawns and manages the am-orchestrator binary for loop instances.
"""

from __future__ import annotations

import asyncio
import logging
import os
import shutil
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING

import httpx

if TYPE_CHECKING:
    from .instance import Instance

log = logging.getLogger(__name__)


@dataclass
class OrchestratorProcess:
    """Tracks a running orchestrator process."""
    title: str
    process: asyncio.subprocess.Process | None = None
    port: int = 0
    task: asyncio.Task | None = None
    _output_lines: list[str] = field(default_factory=list)
    instance: Instance | None = None

    @property
    def is_running(self) -> bool:
        return self.process is not None and self.process.returncode is None

    @property
    def pid(self) -> int | None:
        return self.process.pid if self.process else None


class OrchestratorManager:
    """Manages orchestrator processes for loop instances.

    Each loop instance can have at most one orchestrator process running.
    The manager handles spawning, monitoring, and cleanup.
    """

    def __init__(self, base_url: str = "http://localhost:8787", base_port: int = 9100):
        self._processes: dict[str, OrchestratorProcess] = {}
        self._base_url = base_url
        self._base_port = base_port
        self._port_counter = 0
        self._lock = asyncio.Lock()

    @property
    def base_url(self) -> str:
        return self._base_url

    def find_binary(self) -> str | None:
        return self._find_binary()

    def _find_binary(self) -> str | None:
        """Find the am-orchestrator binary."""
        # Check common locations
        locations = [
            "/usr/local/bin/am-orchestrator",
            "/usr/bin/am-orchestrator",
            str(Path(__file__).resolve().parents[2] / "orchestrator" / "am-orchestrator"),
            shutil.which("am-orchestrator"),
        ]
        for loc in locations:
            if loc and Path(loc).is_file():
                return loc
        return None

    def _allocate_port(self) -> int:
        """Allocate a unique port for an orchestrator instance."""
        self._port_counter += 1
        return self._base_port + self._port_counter

    async def start(
        self,
        instance: Instance,
        children: list[Instance] | None = None,
        *,
        replace: bool = False,
    ) -> OrchestratorProcess:
        """Start an orchestrator process for a loop instance.

        Args:
            instance: The loop instance to orchestrate
            children: Child agent instances

        Returns:
            The OrchestratorProcess tracking the spawned process.

        Raises:
            RuntimeError: If the binary is not found or process fails to start.
        """
        async with self._lock:
            # Serialize duplicate starts as well as stop/restart operations.
            existing = self._processes.get(instance.title)
            if existing and existing.is_running and not replace:
                raise RuntimeError("Team controller already running")
            if existing:
                await self._stop_locked(instance.title)

            binary = self._find_binary()
            if not binary:
                raise RuntimeError("am-orchestrator binary not found; rebuild the container")

            port = self._allocate_port()

            from .orchestration.controllers import launch_environment
            environment = launch_environment(instance, self._base_url)
            mode = "task-queue" if getattr(instance, "controller_mode", None) == "task_queue" else "team"
            # Build command line args
            cmd = [
                binary,
                "--group", instance.title,
                "--base-url", self._base_url,
                "--mode", mode,
                "--mcp-port", str(port),
            ]

            if instance.task:
                cmd.extend(["--task", instance.task])

            log.info("Starting orchestrator for %s: %s", instance.title, " ".join(cmd))

            try:
                process = await asyncio.create_subprocess_exec(
                    *cmd,
                    stdout=asyncio.subprocess.PIPE,
                    stderr=asyncio.subprocess.STDOUT,
                    cwd=instance.path,
                    env={**os.environ, "AM_ORCHESTRATOR_LOG": "info", **environment},
                )
            except Exception as e:
                log.error("Failed to start orchestrator for %s: %s", instance.title, e)
                raise RuntimeError(f"Failed to start orchestrator: {e}") from e

            proc = OrchestratorProcess(
                title=instance.title,
                process=process,
                port=port,
                instance=instance,
            )
            self._processes[instance.title] = proc

            # Start background task to read output
            proc.task = asyncio.create_task(self._read_output(proc))

            # Do not report success for invalid flags, bind failures, or an old binary.
            async with httpx.AsyncClient(timeout=0.5, trust_env=False) as client:
                for _ in range(50):
                    if process.returncode is not None:
                        raise RuntimeError("Orchestrator exited: " + "\n".join(proc._output_lines[-10:]))
                    try:
                        response = await client.get(f"http://127.0.0.1:{port}/status")
                        response.raise_for_status()
                        status = response.json()
                        if status.get("pid") == process.pid and status.get("group") == instance.title:
                            return proc
                    except (httpx.HTTPError, ValueError):
                        pass
                    await asyncio.sleep(0.1)
            await self._stop_locked(instance.title)
            raise RuntimeError("Orchestrator did not become ready")

    async def _read_output(self, proc: OrchestratorProcess) -> None:
        """Read stdout/stderr from the orchestrator process."""
        if not proc.process or not proc.process.stdout:
            return

        try:
            while True:
                line = await proc.process.stdout.readline()
                if not line:
                    break
                text = line.decode("utf-8", errors="replace").rstrip()
                proc._output_lines.append(text)
                if proc.instance is not None and hasattr(proc.instance, "_publish"):
                    await proc.instance._publish({
                        "type": "controller_diagnostic" if getattr(proc.instance, "controller_mode", None) == "task_queue" else "team_event", "actor": "Controller",
                        "event_type": "controller", "text": text,
                    })
                # Keep only last 1000 lines
                if len(proc._output_lines) > 1000:
                    proc._output_lines = proc._output_lines[-500:]
                log.debug("[orchestrator:%s] %s", proc.title, text)
        except asyncio.CancelledError:
            pass
        except Exception as e:
            log.error("Error reading orchestrator output for %s: %s", proc.title, e)

        # Process has exited
        if proc.process:
            await proc.process.wait()
            log.info("Orchestrator for %s exited with code %d", proc.title, proc.process.returncode)
            if proc.instance is not None and hasattr(proc.instance, "_publish"):
                await proc.instance._publish({
                    "type": "controller_diagnostic" if getattr(proc.instance, "controller_mode", None) == "task_queue" else "team_event", "actor": "Controller",
                    "event_type": "controller",
                    "text": f"Controller stopped (exit code {proc.process.returncode})",
                })

    async def stop(self, title: str) -> None:
        """Stop the orchestrator process for a loop instance."""
        async with self._lock:
            await self._stop_locked(title)

    async def _stop_locked(self, title: str) -> None:
        """Stop orchestrator (caller holds lock)."""
        proc = self._processes.pop(title, None)
        if not proc:
            return

        if proc.process and proc.process.returncode is None:
            log.info("Stopping orchestrator for %s (pid=%d)", title, proc.process.pid)
            try:
                proc.process.terminate()
                try:
                    await asyncio.wait_for(proc.process.wait(), timeout=5.0)
                except asyncio.TimeoutError:
                    log.warning("Orchestrator for %s did not terminate, killing", title)
                    proc.process.kill()
                    await proc.process.wait()
            except ProcessLookupError:
                pass  # Already dead

        if proc.task and not proc.task.done():
            proc.task.cancel()
            try:
                await proc.task
            except asyncio.CancelledError:
                pass

    async def restart(
        self,
        instance: Instance,
        children: list[Instance] | None = None,
    ) -> OrchestratorProcess:
        """Restart the orchestrator process for a loop instance."""
        return await self.start(instance, children, replace=True)

    def get(self, title: str) -> OrchestratorProcess | None:
        """Get the orchestrator process for a loop instance."""
        return self._processes.get(title)

    def get_output(self, title: str, lines: int = 50) -> list[str]:
        """Get recent output lines from an orchestrator process."""
        proc = self._processes.get(title)
        if not proc:
            return []
        return proc._output_lines[-lines:]

    async def shutdown(self) -> None:
        """Stop all orchestrator processes."""
        async with self._lock:
            titles = list(self._processes.keys())
        for title in titles:
            await self.stop(title)


# Global manager instance
_manager: OrchestratorManager | None = None


def get_manager(base_url: str = "http://localhost:8787") -> OrchestratorManager:
    """Get or create the global orchestrator manager."""
    global _manager
    if _manager is None:
        _manager = OrchestratorManager(base_url=base_url)
    return _manager
