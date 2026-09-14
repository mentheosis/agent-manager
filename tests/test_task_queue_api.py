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
    profile.write_text(json.dumps({'test': {'database_env': 'QUEUE_TEST_DSN',
        'repositories': {'sample': str(tmp_path)}, 'default_max_workers': 2,
        'tasks': {'neutral': {'instructions': str(tmp_path / 'task.md'), 'provider': 'codex',
                             'model': 'test-model', 'permission': 'dangerFullAccess', 'parameters': {}}}}}))
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
    assert config['initial_max_workers'] == 2
    assert config['workspace_root'] == str(__import__('pathlib').Path(parent.path) / 'state/queue-work/test')
    assert config['controller_id'] == parent.instance_id
    assert 'secret-password' in config['dsn']
    assert client.post('/api/instances', json={'name': 'bad', 'path': '.', 'kind': 'loop',
        'controller_mode': 'task_queue', 'queue_profile': 'test', 'queue_initial_max_workers': 0}).status_code == 400
    stopped = client.get('/api/task-queues/queue/status').json()
    assert stopped['state'] == 'stopped' and stopped['queue'] is None
    assert stopped['settings'] == {'queue_id': 'test', 'max_workers': 2, 'limits': {
        'lease_secs': 60, 'task_limit_secs': 3600, 'task_limit_tokens': 2000000}}
    parent.queue_lease_secs = 120
    assert client.get('/api/task-queues/queue/status').json()['settings']['limits']['lease_secs'] == 120
    assert 'secret' not in json.dumps(stopped)
    assert client.post('/api/instances/queue/orchestrator/start').status_code == 400


def test_attempt_launch_is_idempotent_and_cancellation_fences_late_launch(queue_app, monkeypatch):
    client, app, parent, manager = queue_app
    starts, sends = AsyncMock(), AsyncMock()
    monkeypatch.setattr(Instance, 'start', starts)
    monkeypatch.setattr(Instance, 'send', sends)
    attempt = 'a' * 32
    workspace = __import__('pathlib').Path(parent.path) / 'state' / 'queue-work' / 'test' / 'attempts' / attempt
    workspace.mkdir(parents=True)
    url = f'/api/task-queues/queue/attempts/{attempt}'
    body = {'task_id': 12, 'workspace': str(workspace), 'prompt': 'Neutral instructions', 'execution': {'provider': 'codex', 'model': 'test-model', 'permission': 'dangerFullAccess'}}
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
    assert child.provider == 'codex' and child.model == 'test-model' and child.permission_mode == 'danger-full-access'
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


def test_queue_identity_and_overrides_roundtrip(queue_app):
    client, app, parent, manager = queue_app
    response = client.post('/api/instances', json={'name': 'independent', 'path': parent.path,
        'kind': 'loop', 'controller_mode': 'task_queue', 'queue_profile': 'test',
        'queue_id': 'test', 'queue_initial_max_workers': 3, 'queue_lease_secs': 30, 'queue_task_limit_secs': 120, 'queue_task_limit_tokens': 10000})
    assert response.status_code == 201, response.text
    created = app.state.registry.get(response.json()['title'])
    assert created.queue_id == 'test'
    config = json.loads(launch_environment(created, 'http://localhost')['AM_QUEUE_CONFIG'])
    assert config['initial_max_workers'] == 3 and config['lease_seconds'] == 30 and config['max_attempt_seconds'] == 120 and config['max_attempt_tokens'] == 10000
    assert json.loads(launch_environment(created, 'http://localhost')['AM_QUEUE_CONFIG'])['queue_id'] == 'test'
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


def test_profile_name_is_queue_identifier(queue_app):
    client, app, parent, manager = queue_app
    body = {'name': 'pool-controller', 'path': parent.path, 'kind': 'loop',
            'controller_mode': 'task_queue', 'queue_profile': 'test'}
    assert client.post('/api/instances', json={**body, 'queue_id': 'different'}).status_code == 400
    response = client.post('/api/instances', json=body)
    assert response.status_code == 201, response.text
    assert response.json()['queue_id'] == 'test'


def test_resolved_execution_survives_profile_edit(queue_app, monkeypatch):
    import os
    from pathlib import Path
    client, app, parent, manager = queue_app
    monkeypatch.setattr(Instance, 'start', AsyncMock())
    monkeypatch.setattr(Instance, 'send', AsyncMock())
    # The internal scheduler passes execution settings from the SQL snapshot,
    # which must take precedence over current profile task settings.
    profile = Path(os.environ['AM_TASK_QUEUE_PROFILES'])
    data = json.loads(profile.read_text())
    data['test']['tasks']['neutral']['model'] = 'changed-model'
    profile.write_text(json.dumps(data))
    attempt = 'c' * 32
    config = json.loads(launch_environment(parent, 'http://localhost')['AM_QUEUE_CONFIG'])
    workspace = Path(config['workspace_root']) / 'attempts' / attempt
    workspace.mkdir(parents=True)
    body = {'task_id': 3, 'workspace': str(workspace), 'prompt': 'Saved prompt',
            'execution': {'provider': 'claude', 'model': 'original-model', 'permission': 'bypassPermission'}}
    headers = {'Authorization': 'Bearer ' + internal_token()}
    url = f'/api/task-queues/queue/attempts/{attempt}'
    assert client.post(url, json=body, headers=headers).status_code == 200
    child = app.state.registry.get('queue_' + attempt)
    assert (child.provider, child.model, child.permission_mode) == ('claude', 'original-model', 'bypassPermissions')
    assert client.post(url, json={**body, 'execution': {**body['execution'], 'model': 'other'}}, headers=headers).status_code == 409


def test_shared_workspace_launch_is_restricted_to_approved_repository(queue_app, monkeypatch):
    import os
    from pathlib import Path
    client, app, parent, manager = queue_app
    path=Path(os.environ['AM_TASK_QUEUE_PROFILES'])
    profiles=json.loads(path.read_text());profiles['test']['use_isolated_workspace']=False
    path.write_text(json.dumps(profiles))
    config=json.loads(launch_environment(parent,'http://localhost')['AM_QUEUE_CONFIG'])
    assert config['use_isolated_workspace'] is False
    monkeypatch.setattr(Instance,'start',AsyncMock())
    monkeypatch.setattr(Instance,'send',AsyncMock())
    attempt='d'*32
    inputs=Path(config['workspace_root'])/'attempts'/attempt/'.queue-inputs'
    inputs.mkdir(parents=True)
    body={'task_id':4,'workspace':parent.path,'prompt':'Shared task',
          'execution':{'provider':'codex','permission':'workspace-write'},
          'use_isolated_workspace':False,'repository':'sample'}
    url=f'/api/task-queues/queue/attempts/{attempt}'
    headers={'Authorization':'Bearer '+internal_token()}
    # Fixture's state directory is inside its fake repository. Use a separate
    # approved checkout so the real containment check remains exercised.
    repo=Path(parent.path).parent/(Path(parent.path).name+'-checkout');repo.mkdir()
    monkeypatch.setitem(profiles['test']['repositories'],'sample',str(repo))
    path.write_text(json.dumps(profiles));body['workspace']=str(repo)
    assert client.post(url,json={**body,'workspace':'/tmp'},headers=headers).status_code==400
    assert client.post(url,json={**body,'repository':'unknown'},headers=headers).status_code==400
    response=client.post(url,json=body,headers=headers)
    assert response.status_code==200,response.text
    child=app.state.registry.get('queue_'+attempt)
    assert child.path==str(repo) and child.add_dirs==[str(inputs)]
    assert client.post(url,json={**body,'workspace':'/tmp'},headers=headers).status_code==409
    assert client.post(url,json=body,headers=headers).status_code==200


def test_workspace_flag_defaults_to_isolated_and_rejects_strings(queue_app):
    import os
    from pathlib import Path
    client,app,parent,manager=queue_app
    assert json.loads(launch_environment(parent,'http://localhost')['AM_QUEUE_CONFIG'])['use_isolated_workspace'] is True
    path=Path(os.environ['AM_TASK_QUEUE_PROFILES']);profiles=json.loads(path.read_text())
    profiles['test']['use_isolated_workspace']='false';path.write_text(json.dumps(profiles))
    with pytest.raises(ValueError,match='must be boolean'):
        launch_environment(parent,'http://localhost')


def test_loading_routes_work_when_controller_stopped_and_require_preview(queue_app, monkeypatch):
    from agent_manager.orchestration import task_loading
    client,app,parent,manager=queue_app
    command=AsyncMock(return_value={'task_ids':{'one':12}})
    monkeypatch.setattr(task_loading,'queue_command',command)
    monkeypatch.setattr(manager,'find_binary',lambda:'/tmp/test-orchestrator')
    batch={'batch_key':'test','workflow_id':'run','tasks':[{'key':'one','task_type':'neutral','parameters':{}}]}
    assert client.post('/api/task-queues/queue/enqueue',json=batch).status_code==400
    command.assert_not_awaited()
    assert client.post('/api/task-queues/queue/render',json=batch).status_code==200
    assert command.call_args.args[2]=='render'
    response=client.post('/api/task-queues/queue/enqueue',json={**batch,'preview_hash':'hash'})
    assert response.status_code==200 and response.json()['task_ids']['one']==12
    assert command.call_args.args[2]=='enqueue'


def test_render_route_uses_real_go_renderer_without_database(queue_app, monkeypatch):
    import os
    from pathlib import Path
    binary=os.environ.get('AM_TEST_ORCHESTRATOR_BINARY')
    if not binary: pytest.skip('Requires built orchestrator')
    client,app,parent,manager=queue_app
    Path(parent.path,'task.md').write_text('Inspect the local source. Produce a report.')
    monkeypatch.setattr(manager,'find_binary',lambda:binary)
    response=client.post('/api/task-queues/queue/render',json={'batch_key':'render','workflow_id':'run','tasks':[{'key':'one','task_type':'neutral','parameters':{}}]})
    assert response.status_code==200,response.text
    assert 'Inspect the local source' in response.json()['tasks'][0]['prompt']
    assert 'secret-password' not in response.text


def test_stopped_queue_reads_without_dispatch(queue_app, monkeypatch):
    from agent_manager.orchestration import task_loading
    client, app, parent, manager = queue_app
    command = AsyncMock(return_value=[])
    monkeypatch.setattr(task_loading, 'queue_command', command)
    monkeypatch.setattr(manager, 'find_binary', lambda: '/test/orchestrator')
    for action, query, payload in [('tasks', 'offset=10', {'offset': 10}), ('logs', 'after=20', {'after': 20})]:
        response = client.get(f'/api/task-queues/queue/{action}?{query}')
        assert response.status_code == 200 and response.json() == []
        assert command.call_args.args[2:] == (action, payload)
    assert manager.get(parent.title) is None
