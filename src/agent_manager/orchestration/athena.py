"""Attempt-scoped access to the host's fixed read-only Athena profile."""
import os

import httpx
from fastapi import HTTPException


def athena_arguments(body, jobs, profile):
    if not isinstance(body, dict):
        raise HTTPException(400, 'Athena arguments must be an object')
    action = body.get('action')
    if action == 'query':
        if set(body) - {'action', 'sql', 'max_rows'}:
            raise HTTPException(400, 'Invalid Athena query arguments')
        sql = body.get('sql')
        max_rows = body.get('max_rows', 100)
        if not isinstance(sql, str) or not 0 < len(sql.encode()) <= 65536:
            raise HTTPException(400, 'SQL must be 1..65536 bytes')
        if type(max_rows) is not int or not 1 <= max_rows <= 1000:
            raise HTTPException(400, 'max_rows must be an integer from 1 to 1000')
        if not profile:
            raise HTTPException(503, 'Queue Athena profile is not configured')
        return 'athena_query', {'profile': profile, 'sql': sql, 'max_rows': max_rows}
    if action in ('job_status', 'job_logs'):
        allowed = {'action', 'job_id', 'since_line'} if action == 'job_logs' else {'action', 'job_id'}
        if set(body) - allowed:
            raise HTTPException(400, 'Invalid Athena job arguments')
        job_id = body.get('job_id')
        if not isinstance(job_id, str) or job_id not in jobs:
            raise HTTPException(403, 'Athena job does not belong to this worker')
        args = {'job_id': job_id}
        if action == 'job_logs':
            since_line = body.get('since_line', 1)
            if type(since_line) is not int or since_line < 1:
                raise HTTPException(400, 'since_line must be a positive integer')
            args.update(since_line=since_line, max_lines=200)
        return ('get_job_status' if action == 'job_status' else 'tail_job_log'), args
    raise HTTPException(400, 'Invalid Athena action')


async def host_athena_call(tool, args):
    url = os.environ.get('DOCKER_MCP_URL')
    secret = os.environ.get('DOCKER_MCP_TOKEN')
    if not url or not secret:
        raise HTTPException(503, 'Host MCP is not configured on the backend')
    try:
        async with httpx.AsyncClient(timeout=30, trust_env=False) as client:
            response = await client.post(url.rstrip('/') + '/',
                headers={'Authorization': 'Bearer ' + secret},
                json={'jsonrpc': '2.0', 'id': 1, 'method': 'tools/call',
                      'params': {'name': tool, 'arguments': args}})
            response.raise_for_status()
            result = response.json()
            if not isinstance(result, dict) or not isinstance(result.get('result', result.get('error')), dict):
                raise ValueError('Invalid MCP response')
            return result
    except (httpx.HTTPError, ValueError):
        raise HTTPException(503, 'Host Athena request unavailable; query launch may be uncertain. '
                            'Inspect recorded jobs before submitting the same query again.')
