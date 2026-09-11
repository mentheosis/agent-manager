import json
from types import SimpleNamespace
from unittest.mock import AsyncMock

from fastapi.testclient import TestClient
import pytest

from agent_manager.instance import Instance
from agent_manager.server import build_app
from agent_manager.orchestration.controllers import launch_environment, worker_mcp, internal_token
from agent_manager.persistence import InstanceRecord


@pytest.fixture
def queue_app(tmp_path, monkeypatch):
    import agent_manager.orchestrator as module
    monkeypatch.setenv('AGENT_MANAGER_STATE_DIR', str(tmp_path / 'state'))
    monkeypatch.setenv('QUEUE_TEST_DSN', 'secret-user:secret-password@tcp(db:3306)/queue')
    profile = tmp_path / 'profiles.json'
    profile.write_text(json.dumps({'test': {'queue_id': 'neutral', 'dsn_env': 'QUEUE_TEST_DSN',
        'workspace_root': str(tmp_path / 'work'), 'repositories': {'sample': str(tmp_path)},
        'provider': 'codex', 'model': 'test-model', 'max_workers_ceiling': 2}}))
    monkeypatch.setenv('AM_TASK_QUEUE_PROFILES', str(profile))
    monkeypatch.setattr(module, '_manager', None)
    app = build_app()
    with TestClient(app) as client:
        parent = Instance(title='queue', path=str(tmp_path), kind='loop', controller_mode='task_queue', queue_profile='test')
        app.state.registry._instances[parent.title] = parent
        yield client, app, parent, module._manager
        for instance in app.state.registry.list():
            instance._task = None
        app.state.registry._instances.clear()


def test_profiles_and_launch_config_keep_secrets_server_side(queue_app):
    client, app, parent, manager = queue_app
    response = client.get('/api/task-queue-profiles')
    assert response.status_code == 200
    assert 'secret' not in response.text and 'dsn' not in response.text
    config = json.loads(launch_environment(parent, 'http://localhost:8787')['AM_QUEUE_CONFIG'])
    assert config['initial_max_workers'] == 1
    assert config['controller_id'] == parent.instance_id
    assert 'secret-password' in config['dsn']
    assert client.post('/api/instances', json={'name': 'bad', 'path': '.', 'kind': 'loop',
        'controller_mode': 'task_queue', 'queue_profile': 'test', 'queue_initial_max_workers': 3}).status_code == 400
    assert client.get('/api/task-queues/queue/status').json() == {'state': 'stopped', 'queue': None}
    assert client.post('/api/instances/queue/orchestrator/start').status_code == 400


def test_attempt_launch_is_idempotent_and_cancellation_fences_late_launch(queue_app, monkeypatch):
    client, app, parent, manager = queue_app
    starts, sends = AsyncMock(), AsyncMock()
    monkeypatch.setattr(Instance, 'start', starts)
    monkeypatch.setattr(Instance, 'send', sends)
    attempt = 'a' * 32
    workspace = __import__('pathlib').Path(parent.path) / 'work' / 'attempts' / attempt
    workspace.mkdir(parents=True)
    url = f'/api/task-queues/queue/attempts/{attempt}'
    body = {'task_id': 12, 'workspace': str(workspace), 'prompt': 'Neutral instructions'}
    assert client.post(url, json=body).status_code == 401
    headers = {'Authorization': 'Bearer ' + internal_token()}
    first = client.post(url, json=body, headers=headers)
    assert first.status_code == 200, first.text
    second = client.post(url, json=body, headers=headers)
    assert second.json()['conversation_id'] == first.json()['conversation_id']
    starts.assert_awaited_once()
    sends.assert_awaited_once_with('Neutral instructions')
    assert client.post(url, json={**body, 'prompt': 'different'}, headers=headers).status_code == 409
    child = app.state.registry.get('queue_' + attempt)
    assert child._queue_environment_exclusions() and 'QUEUE_TEST_DSN' in child._queue_environment_exclusions()
    monkeypatch.setattr(manager, '_find_binary', lambda: '/tmp/am-orchestrator')
    mcp = worker_mcp(child, manager)
    assert 'secret-password' not in json.dumps(mcp)
    assert len(mcp['queue']['env']['AM_ATTEMPT_TOKEN']) == 64
    assert client.post(f'/api/instances/{child.title}/send', json={'text': 'untracked work'}).status_code == 400
    assert client.post(f'/api/instances/{child.title}/abort').status_code == 409
    assert client.delete(f'/api/instances/{child.title}').status_code == 409
    # A replacement controller for the same queue can find and cancel this worker.
    other = Instance(title='replacement', path=parent.path, kind='loop', controller_mode='task_queue', queue_profile='test')
    app.state.registry._instances[other.title] = other
    other_url = f'/api/task-queues/replacement/attempts/{attempt}'
    assert client.get(other_url, headers=headers).json()['exists']
    assert client.delete(other_url, headers=headers).status_code == 200
    assert client.post(url, json=body, headers=headers).status_code == 409
    # Cancellation before launch also leaves a durable tombstone.
    late = f'/api/task-queues/queue/attempts/{"b" * 32}'
    assert client.delete(late, headers=headers).status_code == 200
    assert client.post(late, json=body, headers=headers).status_code == 409


def test_old_records_remain_teams_and_queue_identity_roundtrips():
    old = InstanceRecord.from_dict({'title': 'old', 'path': '/tmp', 'kind': 'loop'})
    assert old.controller_mode == 'team'
    record = InstanceRecord(title='q', path='/tmp', kind='loop', controller_mode='task_queue',
                            instance_id='stable', queue_profile='profile', queue_initial_max_workers=2)
    restored = InstanceRecord.from_dict(record.to_dict())
    assert restored.instance_id == 'stable' and restored.queue_initial_max_workers == 2


def test_drain_waits_for_workers_before_stopping(queue_app, monkeypatch):
    import httpx
    import time
    client, app, parent, manager = queue_app
    manager._processes[parent.title] = SimpleNamespace(is_running=True, port=12345)
    manager.stop = AsyncMock()
    reads = 0
    real_client = httpx.AsyncClient
    def handler(request):
        nonlocal reads
        if request.url.path == '/status':
            reads += 1
            return httpx.Response(200, json={'state': 'paused', 'queue': {'paused': True, 'active_workers': 1 if reads == 1 else 0}})
        return httpx.Response(200, json={'ok': True})
    monkeypatch.setattr(httpx, 'AsyncClient', lambda **kwargs: real_client(transport=httpx.MockTransport(handler), **kwargs))
    try:
        response = client.post('/api/task-queues/queue/control', json={'action': 'stop'})
        assert response.status_code == 200 and response.json()['draining']
        deadline = time.monotonic() + 2
        while not manager.stop.await_count and time.monotonic() < deadline:
            time.sleep(.01)
        manager.stop.assert_awaited_once_with('queue')
        assert reads >= 2
    finally:
        manager._processes.clear()


def test_queue_identifier_overrides_profile_and_roundtrips(queue_app):
    client, app, parent, manager = queue_app
    response = client.post('/api/instances', json={'name': 'independent', 'path': parent.path,
        'kind': 'loop', 'controller_mode': 'task_queue', 'queue_profile': 'test',
        'queue_id': 'another-pool', 'queue_initial_max_workers': 1})
    assert response.status_code == 201, response.text
    created = app.state.registry.get(response.json()['title'])
    assert created.queue_id == 'another-pool'
    assert json.loads(launch_environment(created, 'http://localhost')['AM_QUEUE_CONFIG'])['queue_id'] == 'another-pool'
    record = InstanceRecord(title='q', path='/tmp', kind='loop', controller_mode='task_queue', queue_id='another-pool')
    assert InstanceRecord.from_dict(record.to_dict()).queue_id == 'another-pool'


def test_delete_stopped_controller_retains_attempt_conversation(queue_app, monkeypatch):
    client, app, parent, manager = queue_app
    child = Instance(title='retained', path=parent.path, parent=parent.title,
                     queue_profile='test', queue_id='neutral',
                     queue_attempt={'id': 'a' * 32, 'task_id': 1})
    app.state.registry._instances[child.title] = child
    parent.children.append(child.title)
    manager.stop = AsyncMock()
    stop = AsyncMock()
    monkeypatch.setattr(child, 'stop', stop)
    response = client.delete('/api/instances/queue')
    assert response.status_code == 204, response.text
    assert app.state.registry.get('queue') is None
    assert app.state.registry.get('retained') is child
    assert child.parent is None and child.queue_attempt['cancelled']
    stop.assert_awaited_once()


def test_profile_without_queue_registration_requires_ui_identifier(queue_app):
    import os
    from pathlib import Path
    client, app, parent, manager = queue_app
    path = Path(os.environ['AM_TASK_QUEUE_PROFILES'])
    profiles = json.loads(path.read_text())
    profiles['test'].pop('queue_id')
    path.write_text(json.dumps(profiles))
    body = {'name': 'pool-controller', 'path': parent.path, 'kind': 'loop',
            'controller_mode': 'task_queue', 'queue_profile': 'test'}
    assert client.post('/api/instances', json=body).status_code == 400
    response = client.post('/api/instances', json={**body, 'queue_id': 'unregistered-pool'})
    assert response.status_code == 201, response.text
    assert response.json()['queue_id'] == 'unregistered-pool'
