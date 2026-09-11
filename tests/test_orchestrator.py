from __future__ import annotations

import asyncio
import json
import os
import socket
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace

import httpx
import pytest

from agent_manager.instance import Instance
from agent_manager.orchestrator import OrchestratorManager
from agent_manager.providers.base import AgentConfig
from agent_manager.providers.codex import CodexRuntime


async def test_missing_binary_is_failure(tmp_path, monkeypatch):
    manager = OrchestratorManager()
    monkeypatch.setattr(manager, '_find_binary', lambda: None)
    with pytest.raises(RuntimeError, match='binary not found'):
        await manager.start(SimpleNamespace(title='team', path=str(tmp_path), task='work'))


async def test_failed_start_reports_output(tmp_path, monkeypatch):
    binary = tmp_path / 'old-binary'
    binary.write_text('#!/bin/sh\necho "flag provided but not defined: -mode"\nexit 2\n')
    binary.chmod(0o755)
    manager = OrchestratorManager()
    monkeypatch.setattr(manager, '_find_binary', lambda: str(binary))
    with pytest.raises(RuntimeError, match='flag provided but not defined'):
        await manager.start(SimpleNamespace(title='team', path=str(tmp_path), task='work'))
    await manager.shutdown()


async def test_stop_terminates_before_waiting_for_output_reader():
    from agent_manager.orchestrator import OrchestratorProcess
    process = await asyncio.create_subprocess_exec(
        sys.executable, '-c', 'import time; time.sleep(60)',
        stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.STDOUT,
    )
    manager = OrchestratorManager()
    proc = OrchestratorProcess('team', process=process)
    manager._processes['team'] = proc
    proc.task = asyncio.create_task(manager._read_output(proc))
    await asyncio.wait_for(manager.stop('team'), 2)
    assert process.returncode is not None
    assert proc.task.done()


@pytest.mark.parametrize('resumed', [False, True])
def test_codex_team_tools_fresh_and_resume(resumed):
    config = AgentConfig(title='leader', provider='codex', cwd='/tmp',
        session_id='session' if resumed else None,
        team_mcp={'command': '/usr/local/bin/am-orchestrator',
                  'args': ['--mode', 'mcp', '--managed', '--group', 'team']})
    command = CodexRuntime(config)._build_command('work', [])
    assert 'mcp_servers.team.command="/usr/local/bin/am-orchestrator"' in command
    assert 'mcp_servers.team.args=["--mode", "mcp", "--managed", "--group", "team"]' in command


def test_only_team_leader_gets_tools(monkeypatch):
    import agent_manager.orchestrator as module
    manager = OrchestratorManager()
    monkeypatch.setattr(manager, '_find_binary', lambda: '/test/am-orchestrator')
    monkeypatch.setattr(module, '_manager', manager)
    inst = Instance(title='leader', path='/tmp', parent='team', agent_preset='orchestrator')
    assert '--managed' in inst._team_mcp_config()['args']
    inst.agent_preset = 'coder'
    assert inst._team_mcp_config() is None


async def test_claude_preserves_docker_and_adds_team(monkeypatch):
    import agent_manager.providers.claude as module
    captured = {}
    class Client:
        def __init__(self, options): captured['options'] = options
        async def __aenter__(self): return self
        async def __aexit__(self, *args): pass
        async def receive_messages(self):
            if False: yield None
    monkeypatch.setattr(module, 'ClaudeSDKClient', Client)
    monkeypatch.setenv('DOCKER_MCP_URL', 'http://docker.invalid')
    monkeypatch.setenv('DOCKER_MCP_TOKEN', 'test-token')
    tools = {'command': '/test/am-orchestrator', 'args': ['--group', 'team']}
    runtime = module.ClaudeRuntime(AgentConfig(title='leader', provider='claude', cwd='/tmp', team_mcp=tools))
    await runtime.start()
    assert captured['options'].mcp_servers['team'] == tools
    assert 'docker' in captured['options'].mcp_servers
    await runtime.close()


async def test_python_launcher_with_real_go_controller(tmp_path, monkeypatch):
    binary = os.environ.get('AM_TEST_ORCHESTRATOR_BINARY')
    if not binary:
        pytest.skip('set AM_TEST_ORCHESTRATOR_BINARY to a freshly built binary')
    prompts = []
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args): pass
        def do_GET(self):
            self.send_response(200); self.end_headers()
            self.wfile.write(json.dumps([
                {'title': 'leader', 'status': 'ready', 'agent_preset': 'orchestrator'},
                {'title': 'worker', 'status': 'ready'},
            ]).encode())
        def do_POST(self):
            prompts.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
            self.send_response(200); self.end_headers(); self.wfile.write(b'{}')
    api = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    thread = threading.Thread(target=api.serve_forever, daemon=True); thread.start()
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0)); port = sock.getsockname()[1]
    manager = OrchestratorManager(f'http://127.0.0.1:{api.server_port}', base_port=port-1)
    monkeypatch.setattr(manager, '_find_binary', lambda: binary)
    try:
        proc = await manager.start(SimpleNamespace(title='team', path=str(tmp_path), task='test task'))
        assert proc.is_running
        with pytest.raises(RuntimeError, match='already running'):
            await manager.start(SimpleNamespace(title='team', path=str(tmp_path), task='duplicate'))
        async with httpx.AsyncClient(trust_env=False) as client:
            for _ in range(50):
                response = await client.get(f'http://127.0.0.1:{proc.port}/status')
                if response.json()['state'] == 'running': break
                await asyncio.sleep(.02)
            assert response.json()['state'] == 'running'
        assert len(prompts) == 1
        assert 'test task' in prompts[0]['text']
        await asyncio.wait_for(manager.stop('team'), 2)
        assert not proc.is_running
    finally:
        await manager.shutdown()
        api.shutdown(); api.server_close(); thread.join()
