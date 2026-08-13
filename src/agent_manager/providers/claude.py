from __future__ import annotations

import asyncio
import logging
import os
from collections.abc import AsyncIterator
from typing import Any

from claude_agent_sdk import (
    AssistantMessage,
    ClaudeAgentOptions,
    ClaudeSDKClient,
    ResultMessage,
    SystemMessage,
    TextBlock,
    ThinkingBlock,
    ToolResultBlock,
    ToolUseBlock,
    UserMessage,
)

from agent_manager.artifacts import artifact_instruction

from .base import (
    AgentConfig,
    AgentEvent,
    AgentInput,
    BaseRuntime,
    build_prompt_with_context,
    format_memory_block,
)

log = logging.getLogger(__name__)


class ClaudeRuntime(BaseRuntime):
    provider = "claude"

    def __init__(self, config: AgentConfig) -> None:
        super().__init__()
        self.config = config
        self._client_cm: ClaudeSDKClient | None = None
        self._client: ClaudeSDKClient | None = None
        # Persistent pump task that reads receive_messages() and pushes translated
        # events onto the shared queue. One pump per runtime lifetime.
        self._pump_task: asyncio.Task[None] | None = None

    async def start(self) -> None:
        opts: dict[str, Any] = {
            "cwd": self.config.cwd,
            "permission_mode": self.config.permission_mode,
        }
        if self.config.session_id:
            # Continue the prior conversation. CLI loads its persisted session jsonl.
            opts["resume"] = self.config.session_id
        if self.config.add_dirs:
            opts["add_dirs"] = list(self.config.add_dirs)
        if self.config.model:
            opts["model"] = self.config.model

        # Move stable per-instance instructions (memory file + artifact protocol)
        # into the system prompt rather than per-turn user prompts. This puts them
        # in the cacheable prefix (Anthropic prompt caching, 5-min TTL, 90% discount
        # on hits) so we pay ~0.1x token cost per subsequent turn instead of 1.0x.
        # Uses the SDK's "append" preset feature which adds our content after
        # Claude Code's default system prompt.
        append_parts: list[str] = []
        memory_block = format_memory_block(self.config.memory_file)
        if memory_block:
            append_parts.append(memory_block)
        # Always include the artifact instruction — it's stable per build.
        append_parts.append(artifact_instruction().strip())

        if append_parts:
            append_text = "\n\n".join(append_parts)
            log.info(
                "instance %s: appending %d chars to system prompt (memory=%s)",
                self.config.title,
                len(append_text),
                "yes" if memory_block else "no",
            )
            opts["system_prompt"] = {
                "type": "preset",
                "preset": "claude_code",
                "append": append_text,
            }

        docker_mcp_url = os.environ.get("DOCKER_MCP_URL")
        docker_mcp_token = os.environ.get("DOCKER_MCP_TOKEN")
        if docker_mcp_url and docker_mcp_token:
            opts["mcp_servers"] = {
                "docker": {
                    "type": "http",
                    "url": docker_mcp_url.rstrip("/") + "/",
                    "headers": {
                        "Authorization": f"Bearer {docker_mcp_token}",
                    },
                },
            }

        options = ClaudeAgentOptions(**opts)
        log.info(
            "instance %s: starting Claude SDK client (session_id=%s)",
            self.config.title,
            self.config.session_id,
        )
        client_cm = ClaudeSDKClient(options=options)
        client = await client_cm.__aenter__()
        self._client_cm = client_cm
        self._client = client

        # Spawn the persistent message pump. It reads receive_messages() (the
        # SDK's continuous stream, distinct from receive_response() which stops
        # at ResultMessage) and pushes translated events onto our shared queue.
        # This is what lets async events emitted AFTER a turn's result reach
        # the UI in real time instead of queuing until the next user prompt.
        self._pump_task = asyncio.create_task(
            self._pump_messages(client),
            name=f"claude-pump:{self.config.title}",
        )

    async def _pump_messages(self, client: ClaudeSDKClient) -> None:
        """Continuously translate SDK messages into events on the shared queue."""
        try:
            async for msg in client.receive_messages():
                for event in translate_claude_message(msg):
                    await self._emit_event(event)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            log.exception("instance %s: Claude pump crashed", self.config.title)
            await self._emit_event({
                "type": "error",
                "message": f"Claude event pump failed: {type(e).__name__}: {e}",
            })

    async def query(self, message: AgentInput) -> None:
        """Send one message to Claude. Events arrive via event_stream()."""
        if self._client is None:
            raise RuntimeError("Claude runtime not started")

        if message.images:
            log.info(
                "instance %s: multimodal message with %d images, text=%d chars",
                self.config.title,
                len(message.images),
                len(message.text),
            )
            prompt = self._build_multimodal_content(
                self._prompt_with_context(message.text), message.images,
            )
        else:
            prompt = self._prompt_with_context(message.text)

        log.info(
            "instance %s: calling Claude client.query() with prompt type=%s",
            self.config.title,
            type(prompt).__name__,
        )
        await self._client.query(prompt)
        log.info("instance %s: Claude client.query() returned", self.config.title)

    async def close(self) -> None:
        if self._pump_task and not self._pump_task.done():
            self._pump_task.cancel()
            try:
                await self._pump_task
            except asyncio.CancelledError:
                pass
            except Exception:
                log.exception("instance %s: pump task raised during close", self.config.title)
        self._pump_task = None

        if self._client_cm is not None:
            try:
                await self._client_cm.__aexit__(None, None, None)
            except Exception:
                log.exception("instance %s: SDK client close raised", self.config.title)
        self._client_cm = None
        self._client = None

    async def _build_multimodal_content(
        self, text: str, images: list[dict[str, Any]]
    ) -> AsyncIterator[dict[str, Any]]:
        """Build an async iterator for Claude streaming input mode with images."""
        content: list[dict[str, Any]] = []
        for img in images:
            content.append({
                "type": "image",
                "source": {
                    "type": "base64",
                    "media_type": img["media_type"],
                    "data": img["data"],
                },
            })
        if text:
            content.append({"type": "text", "text": text})

        log.info(
            "instance %s: yielding Claude user message with %d content blocks",
            self.config.title,
            len(content),
        )
        yield {
            "type": "user",
            "message": {
                "role": "user",
                "content": content,
            },
        }

    def _prompt_with_context(self, text: str) -> str:
        """Build the per-turn prompt.

        For Claude, both memory_file content and the artifact instruction live in
        the system prompt at startup (cacheable, ~90% token discount on hits).
        The per-turn prompt is therefore just the user's actual input.
        """
        return build_prompt_with_context(
            text,
            memory_file=None,
            include_artifact_instruction=False,
        )


def translate_claude_message(msg: Any) -> list[AgentEvent]:
    events: list[AgentEvent] = []
    if isinstance(msg, AssistantMessage):
        for block in msg.content:
            if isinstance(block, TextBlock):
                events.append({"type": "assistant_text", "text": block.text})
            elif isinstance(block, ThinkingBlock):
                events.append({"type": "thinking", "text": getattr(block, "thinking", "")})
            elif isinstance(block, ToolUseBlock):
                events.append(
                    {
                        "type": "tool_use",
                        "id": block.id,
                        "name": block.name,
                        "input": block.input,
                    }
                )
        # Surface the per-turn usage so the context monitor measures LIVE context
        # occupancy. AssistantMessage.usage is THIS call's prompt size (input +
        # cache_read + cache_creation), which plateaus at the true context size — unlike
        # the CUMULATIVE ResultMessage.usage, which grows unbounded and trips any split
        # threshold on any window. See ROOT-CAUSE-cumulative-usage.md.
        usage = getattr(msg, "usage", None)
        if isinstance(usage, dict):
            events.append({"type": "assistant_usage", "usage": usage})
    elif isinstance(msg, UserMessage):
        content = msg.content
        if isinstance(content, list):
            for block in content:
                if isinstance(block, ToolResultBlock):
                    output = block.content
                    if not isinstance(output, str):
                        output = str(output)
                    events.append(
                        {
                            "type": "tool_result",
                            "tool_id": block.tool_use_id,
                            "output": output,
                            "is_error": bool(getattr(block, "is_error", False)),
                        }
                    )
    elif isinstance(msg, ResultMessage):
        usage = getattr(msg, "usage", None)
        events.append(
            {
                "type": "result",
                "subtype": getattr(msg, "subtype", None),
                "duration_ms": getattr(msg, "duration_ms", None),
                "num_turns": getattr(msg, "num_turns", None),
                "total_cost_usd": getattr(msg, "total_cost_usd", None),
                "is_error": bool(getattr(msg, "is_error", False)),
                "session_id": getattr(msg, "session_id", None),
                "usage": usage if isinstance(usage, dict) else None,
            }
        )
    elif isinstance(msg, SystemMessage):
        subtype = getattr(msg, "subtype", None)
        if subtype == "init":
            events.append({"type": "system_init", "data": getattr(msg, "data", {})})
    return events
