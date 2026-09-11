from types import SimpleNamespace
from unittest.mock import AsyncMock

import httpx
from fastapi.testclient import TestClient

from agent_manager.instance import Instance
from agent_manager.orchestrator import OrchestratorManager
from agent_manager.server import build_app


def test_team_api_lifecycle_and_completion(tmp_path, monkeypatch):
    import agent_manager.orchestrator as module
    monkeypatch.setenv('AGENT_MANAGER_STATE_DIR', str(tmp_path))
    monkeypatch.setenv('AGENT_MANAGER_PORT', '9123')
    monkeypatch.setattr(module, '_manager', None)
    app = build_app()
    manager = module._manager
    assert manager._base_url == 'http://127.0.0.1:9123'
    monkeypatch.setattr(manager, '_find_binary', lambda: '/test/am-orchestrator')
    process = SimpleNamespace(is_running=True, pid=123, port=9101)
    manager.start = AsyncMock(return_value=process)
    manager.restart = AsyncMock(return_value=process)
    manager.stop = AsyncMock()
    calls = []
    real_client = httpx.AsyncClient
    def handler(request):
        calls.append(request)
        if request.method == 'GET':
            return httpx.Response(200, json={'state': 'done'})
        return httpx.Response(200, json={'status': 'ok'})
    monkeypatch.setattr(httpx, 'AsyncClient', lambda **kwargs: real_client(transport=httpx.MockTransport(handler), **kwargs))
    with TestClient(app) as client:
        parent = Instance(title='team', path=str(tmp_path), kind='loop', task='work', children=['leader', 'worker'])
        leader = Instance(title='leader', path=str(tmp_path), parent='team', agent_preset='orchestrator')
        worker = Instance(title='worker', path=str(tmp_path), parent='team')
        leader.status = 'ready'
        leader.reload_options = AsyncMock()
        app.state.registry._instances.update(team=parent, leader=leader, worker=worker)
        response = client.post('/api/instances/team/orchestrator/start')
        assert response.status_code == 200, response.text
        leader.reload_options.assert_awaited_once()
        manager.start.assert_awaited_once()
        manager._processes['team'] = process
        assert client.post('/api/instances/team/orchestrator/start').status_code == 409
        assert client.post('/api/instances/team/orchestrator/pause').status_code == 200
        assert calls[-1].url.path == '/pause'
        assert client.post('/api/instances/team/orchestrator/resume').status_code == 200
        assert calls[-1].url.path == '/resume'
        assert client.post('/api/instances/team/orchestrator/complete', json={'summary': 'finished'}).status_code == 200
        assert b'mark_task_done' in calls[-1].content
        assert client.get('/api/instances/team/orchestrator/status').json()['state'] == 'done'
        assert client.post('/api/instances/team/orchestrator/complete', json={}).status_code == 400
        leader.status = 'running'
        assert client.post('/api/instances/team/orchestrator/restart').status_code == 409
        manager._processes.clear()
        app.state.registry._instances.clear()


def test_start_requires_leader_worker_and_task(tmp_path, monkeypatch):
    import agent_manager.orchestrator as module
    monkeypatch.setenv('AGENT_MANAGER_STATE_DIR', str(tmp_path))
    monkeypatch.setattr(module, '_manager', OrchestratorManager())
    app = build_app()
    with TestClient(app) as client:
        parent = Instance(title='team', path=str(tmp_path), kind='loop')
        app.state.registry._instances['team'] = parent
        assert client.post('/api/instances/team/orchestrator/start').status_code == 400
        leader = Instance(title='leader', path=str(tmp_path), parent='team', agent_preset='orchestrator')
        app.state.registry._instances['leader'] = leader
        parent.children.append('leader')
        response = client.post('/api/instances/team/orchestrator/start')
        assert response.status_code == 400 and 'worker' in response.json()['detail']
        parent.children.append('worker')
        app.state.registry._instances['worker'] = Instance(title='worker', path=str(tmp_path), parent='team')
        response = client.post('/api/instances/team/orchestrator/start')
        assert response.status_code == 400 and 'task' in response.json()['detail']
        app.state.registry._instances.clear()
