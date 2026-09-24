"""Recover MCP identity omitted from older compact Codex events, without rewriting logs."""
from functools import lru_cache
import json
import os
from pathlib import Path


def _key(arguments):
    if isinstance(arguments, str):
        try:
            arguments = json.loads(arguments)
        except ValueError:
            pass
    return json.dumps(arguments, sort_keys=True, ensure_ascii=False)


@lru_cache(maxsize=16)
def _identities(path, modified, size):
    by_arguments = {}
    with open(path) as source:
        for line in source:
            try:
                raw = json.loads(line)
                payload = raw.get('payload', {})
                item = payload.get('item', {})
                if item.get('type') != 'McpToolCall':
                    if payload.get('type') != 'mcp_tool_call_end':
                        continue
                    item = payload.get('invocation', {})
                server, tool = item.get('server'), item.get('tool')
                if not isinstance(server, str) or not isinstance(tool, str):
                    continue
                by_arguments.setdefault(_key(item.get('arguments', {})), set()).add((server, tool))
            except (ValueError, AttributeError, TypeError):
                continue
    return by_arguments


def enrich_mcp_history(events, session_id):
    if not session_id or not any(e.get('name') == 'mcp_tool_call' for e in events):
        return events
    # Session identity is a provider UUID, never an arbitrary glob/path.
    if any(c not in '0123456789abcdef-' for c in session_id.lower()):
        return events
    home = Path(os.environ.get('CODEX_HOME') or Path.home() / '.codex')
    try:
        paths = sorted((home / 'sessions').glob(f'**/*{session_id}*.jsonl'))
        if not paths:
            return events
        path = paths[-1]
        stat = path.stat()
        identities = _identities(str(path), stat.st_mtime_ns, stat.st_size)
    except OSError:
        return events
    result = []
    for event in events:
        if event.get('type') == 'tool_use' and event.get('name') == 'mcp_tool_call':
            matches = identities.get(_key(event.get('input')), set())
            # Never guess if different tools used the same arguments in this session.
            if len(matches) == 1:
                server, tool = next(iter(matches))
                event = {**event, 'name': f'mcp.{server}.{tool}', 'mcp_server': server, 'mcp_tool': tool}
        result.append(event)
    return result
