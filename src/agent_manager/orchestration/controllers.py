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


def normalize_worker(task: dict) -> dict:
    provider = task.get('provider', 'codex')
    permission = task.get('permission') or task.get('permission_mode') or ('acceptEdits' if provider == 'claude' else 'workspace-write')
    permission = {'dangerFullAccess': 'danger-full-access', 'bypassPermission': 'bypassPermissions'}.get(permission, permission)
    allowed = {'codex': {'read-only', 'workspace-write', 'danger-full-access'},
               'claude': {'default', 'acceptEdits', 'plan', 'bypassPermissions'}}
    if provider not in allowed or permission not in allowed[provider]:
        raise ValueError('Invalid task provider or permission')
    return {'provider': provider, 'model': task.get('model'), 'permission_mode': permission}


def queue_config(name: str) -> dict:
    import re
    if not re.fullmatch(r'[A-Za-z0-9_-]{1,128}', name):
        raise ValueError('Invalid queue profile name')
    profile = dict(profiles().get(name) or {})
    if not profile:
        raise ValueError('Unknown task queue profile')
    env = profile.get('database_env', '')
    if not env or env not in os.environ:
        raise ValueError('Queue database environment variable is not configured')
    use_isolated = profile.get('use_isolated_workspace', True)
    if type(use_isolated) is not bool:
        raise ValueError('use_isolated_workspace must be boolean')
    tasks = profile.get('tasks', {})
    if not isinstance(tasks, dict):
        raise ValueError('Profile tasks must be an object')
    config = {'profile_path': os.environ['AM_TASK_QUEUE_PROFILES'], 'queue_id': name, 'dsn': os.environ[env], 'internal_token': internal_token(),
        'repositories': profile.get('repositories', {}), 'tasks': tasks, 'use_isolated_workspace': use_isolated,
        'workspace_root': str(Path(os.environ.get('AGENT_MANAGER_STATE_DIR', '/var/lib/agent-manager')).resolve() / 'queue-work' / name),
        'initial_max_workers': profile.get('default_max_workers', 1),
        'lease_seconds': profile.get('default_lease_secs', 60),
        'max_attempt_seconds': profile.get('default_task_limit_secs', 3600),
        'max_attempt_tokens': profile.get('default_task_limit_tokens', 2000000)}
    validate_limits(config)
    return config


def validate_limits(config: dict):
    for key in ('initial_max_workers', 'lease_seconds', 'max_attempt_seconds', 'max_attempt_tokens'):
        if type(config[key]) is not int or config[key] < 1:
            raise ValueError('Queue limits must be positive integers')
    if config['lease_seconds'] < 15 or config['max_attempt_seconds'] < config['lease_seconds']:
        raise ValueError('Lease must be at least 15 seconds and no longer than task time limit')


def instance_queue_config(instance) -> dict:
    return queue_config(instance.queue_profile or instance.queue_id)


def launch_environment(instance, base_url: str) -> dict:
    if getattr(instance, 'controller_mode', 'team') != 'task_queue':
        return {}
    config = instance_queue_config(instance)
    for field, key in [('queue_initial_max_workers', 'initial_max_workers'), ('queue_lease_secs', 'lease_seconds'),
                       ('queue_task_limit_secs', 'max_attempt_seconds'), ('queue_task_limit_tokens', 'max_attempt_tokens')]:
        value = getattr(instance, field, None)
        if value is not None:
            config[key] = value
    validate_limits(config)
    config.update(base_url=base_url, parent=instance.title, controller_id=instance.instance_id)
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
                 *(p.get('database_env', '') for p in profiles().values())} - {''})
