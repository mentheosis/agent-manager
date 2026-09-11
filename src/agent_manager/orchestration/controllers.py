from __future__ import annotations

import hashlib
import hmac
import json
import os
import secrets
from pathlib import Path


def internal_token() -> str:
    """Stable across app/controller restarts; never exposed in UI or transcripts."""
    path = Path(os.environ.get('AGENT_MANAGER_STATE_DIR', '/var/lib/agent-manager')) / 'queue-api-token'
    path.parent.mkdir(parents=True, exist_ok=True)
    try:
        fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        return path.read_text().strip()
    token = secrets.token_hex(32)
    with os.fdopen(fd, 'w') as file:
        file.write(token)
    return token


def attempt_token(attempt_id: str) -> str:
    return hmac.new(internal_token().encode(), ('queue-attempt:' + attempt_id).encode(), hashlib.sha256).hexdigest()


def profiles() -> dict:
    path = os.environ.get('AM_TASK_QUEUE_PROFILES')
    if not path:
        return {}
    data = json.loads(Path(path).read_text())
    if not isinstance(data, dict):
        raise ValueError('Task queue profiles must be an object')
    return data


def queue_config(name: str) -> dict:
    profile = dict(profiles().get(name) or {})
    if not profile:
        raise ValueError('Unknown task queue profile')
    env = profile.pop('dsn_env', '')
    if not env or env not in os.environ:
        raise ValueError('Queue database environment variable is not configured')
    profile['dsn'] = os.environ[env]
    profile['internal_token'] = internal_token()
    if not Path(profile.get('workspace_root', '')).is_absolute():
        raise ValueError('Queue workspace_root must be absolute')
    if profile.get('provider', 'codex') not in ('codex', 'claude'):
        raise ValueError('Invalid queue worker provider')
    return profile


def instance_queue_config(instance) -> dict:
    config = queue_config(instance.queue_profile)
    config['queue_id'] = instance.queue_id or config.get('queue_id', '')
    return config


def launch_environment(instance, base_url: str) -> dict:
    if getattr(instance, 'controller_mode', 'team') != 'task_queue':
        return {}
    config = instance_queue_config(instance)
    config.update(initial_max_workers=instance.queue_initial_max_workers, base_url=base_url, parent=instance.title, controller_id=instance.instance_id)
    return {'AM_QUEUE_CONFIG': json.dumps(config)}


def worker_mcp(instance, manager) -> dict:
    attempt = getattr(instance, 'queue_attempt', None)
    if not attempt:
        return {}
    binary = manager.find_binary()
    if not binary:
        raise RuntimeError('am-orchestrator binary is unavailable')
    return {'queue': {'command': binary,
        'args': ['--mode', 'queue-worker', '--group', instance.parent,
                 '--base-url', manager.base_url, '--attempt', attempt['id']],
        'env': {'AM_ATTEMPT_TOKEN': attempt_token(attempt['id'])}}}


def worker_environment_exclusions() -> list[str]:
    return list({'AM_QUEUE_CONFIG', 'AM_TASK_QUEUE_PROFILES', 'DOCKER_MCP_TOKEN', 'DOCKER_MCP_URL',
                 *(p.get('dsn_env', '') for p in profiles().values())} - {''})
