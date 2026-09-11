"""Team-specific leader runtime configuration."""
def leader_mcp(instance, manager):
    if not instance.parent or instance.agent_preset != 'orchestrator' or instance.queue_attempt:
        return None
    binary = manager.find_binary()
    if not binary:
        return None
    return {'command': binary, 'args': ['--mode', 'mcp', '--managed', '--group', instance.parent,
                                       '--base-url', manager.base_url]}
