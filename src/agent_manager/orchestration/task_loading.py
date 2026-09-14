"""Operator render/enqueue adapter. All template and SQL behavior lives in Go."""
import asyncio
import json
import os


async def queue_command(config, binary, action, payload):
    if action not in ('render', 'enqueue', 'tasks', 'logs'):
        raise ValueError('Unknown queue command')
    raw = json.dumps(payload).encode()
    if len(raw) > 1 << 20:
        raise ValueError('Batch exceeds 1 MiB')
    if not binary:
        raise ValueError('Rebuild/install am-orchestrator first')
    proc = await asyncio.create_subprocess_exec(
        binary, '--mode', 'task-queue', '--queue-action', action,
        stdin=asyncio.subprocess.PIPE, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
        env={**os.environ, 'AM_QUEUE_CONFIG': json.dumps(config)})
    try:
        stdout, stderr = await asyncio.wait_for(proc.communicate(raw), timeout=60)
    except BaseException:
        if proc.returncode is None:
            proc.kill()
        await proc.wait()
        raise
    if proc.returncode:
        # SQL/OS errors may carry deployment details. Return validation errors,
        # but never expose the connection string or database authentication text.
        error = stderr.decode(errors='replace')[:4000]
        if any(word in error.lower() for word in ('access denied', 'password', 'tcp(', 'dial tcp')):
            raise ValueError('Queue database unavailable or credentials invalid')
        error = error.replace(config['dsn'], '[database]').replace(config['internal_token'], '[token]')
        raise ValueError(error.strip() or 'Queue command failed')
    return json.loads(stdout)
