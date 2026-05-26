from __future__ import annotations

from collections.abc import Callable

from .base import AgentConfig, AgentRuntime
from .claude import ClaudeRuntime
from .codex import CodexRuntime

RuntimeFactory = Callable[[AgentConfig], AgentRuntime]


class ProviderRegistry:
    def __init__(self) -> None:
        self._factories: dict[str, RuntimeFactory] = {}

    def register(self, provider: str, factory: RuntimeFactory) -> None:
        self._factories[provider] = factory

    def create_runtime(self, config: AgentConfig) -> AgentRuntime:
        try:
            factory = self._factories[config.provider]
        except KeyError as e:
            raise ValueError(f"provider not supported yet: {config.provider}") from e
        return factory(config)


default_registry = ProviderRegistry()
default_registry.register("claude", ClaudeRuntime)
default_registry.register("codex", CodexRuntime)
