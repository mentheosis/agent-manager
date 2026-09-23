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
        'lease_secs': 60, 'task_limit_secs': 3600, 'task_limit_tokens': 2000000, 'review_round_limit': 8}}
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
    body = {'task_id': 12, 'workflow_id': 'trx-pilot-001', 'attempt_number': 2, 'workspace': str(workspace), 'prompt': 'Neutral instructions', 'execution': {'provider': 'codex', 'model': 'test-model', 'permission': 'dangerFullAccess'}}
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
    assert child.display_title == 'trx-pilot-001-12-2 ' + attempt[:8]
    assert child.provider == 'codex' and child.model == 'test-model' and child.permission_mode == 'danger-full-access'
    assert child._queue_environment_exclusions() and 'QUEUE_TEST_DSN' in child._queue_environment_exclusions()
    monkeypatch.setattr(manager, '_find_binary', lambda: '/tmp/am-orchestrator')
    mcp = worker_mcp(child, manager)
    assert 'secret-password' not in json.dumps(mcp)
    assert len(mcp['queue']['env']['AM_ATTEMPT_TOKEN']) == 64
    assert client.post(f'/api/instances/{child.title}/send', json={'text': 'untracked work'}).status_code == 409
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
        'queue_id': 'test', 'queue_initial_max_workers': 3, 'queue_lease_secs': 30, 'queue_task_limit_secs': 120, 'queue_task_limit_tokens': 10000, 'queue_review_round_limit': 24})
    assert response.status_code == 201, response.text
    created = app.state.registry.get(response.json()['title'])
    assert created.queue_id == 'test'
    config = json.loads(launch_environment(created, 'http://localhost')['AM_QUEUE_CONFIG'])
    assert config['review_round_limit'] == 24
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

@pytest.mark.parametrize('status,expected', [('running',409),('submitted',409),('completed',204),('blocked',204),('awaiting_review',204),('',204)])
def test_delete_finished_queue_conversation_preserves_queue_records(queue_app, monkeypatch, status, expected):
    from agent_manager.orchestration import task_loading
    client, app, parent, manager = queue_app
    child = Instance(title='finished-worker', path=parent.path, parent=parent.title, status='ready',
        queue_profile='test', queue_attempt={'id':'b'*32,'task_id':99})
    app.state.registry._instances[child.title] = child
    query = AsyncMock(return_value={'status':status})
    monkeypatch.setattr(task_loading, 'queue_command', query)
    monkeypatch.setattr(manager, 'find_binary', lambda: '/test/orchestrator')
    protection = client.get('/api/instances/finished-worker/kill-status').json()
    assert protection['allowed'] == (expected == 204)
    assert bool(protection['reason']) == (expected == 409)
    response = client.delete('/api/instances/finished-worker')
    assert response.status_code == expected, response.text
    assert query.call_args.args[2] == 'attempt-status'
    assert (app.state.registry.get(child.title) is None) == (expected == 204)


def test_replay_relay_requires_task_opt_in_and_scopes_jobs(queue_app, monkeypatch):
    from agent_manager.orchestration.controllers import attempt_token
    from agent_manager.orchestration import task_loading
    client, app, parent, manager = queue_app
    attempt = 'c'*32
    child = Instance(title='replay-worker', path=parent.path, parent=parent.title,
        queue_profile='test', status='ready', queue_attempt={'id':attempt,'task_id':77})
    app.state.registry._instances[child.title] = child
    url = f'/api/task-queues/queue/attempts/{attempt}/replay'
    assert client.post(url,json={'action':'query','sql':'SELECT 1'}).status_code == 401
    headers={'Authorization':'Bearer '+attempt_token(attempt)}
    assert client.post(url,headers=headers,json={'action':'query','sql':'SELECT 1'}).status_code == 403
    child.queue_attempt['replay_profile']='approved-replay'
    monkeypatch.setattr(task_loading,'queue_command',AsyncMock(return_value={'status':'running'}))
    monkeypatch.setattr(manager,'find_binary',lambda:'/test/orchestrator')
    assert client.post(url,headers=headers,json={'action':'job_logs','job_id':'foreign'}).status_code == 403
    assert client.post(url,headers=headers,json={'action':'start','run_key':'../bad','operation':'extract'}).status_code == 400
    assert client.post(url,headers=headers,json={'action':'query','sql':'SELECT 1','profile':'shell'}).status_code == 400
    monkeypatch.setattr(task_loading,'queue_command',AsyncMock(return_value={'status':'completed'}))
    assert client.post(url,headers=headers,json={'action':'query','sql':'SELECT 1'}).status_code == 409


def test_review_turns_share_workspace_but_have_independent_launch_identity(queue_app, monkeypatch):
    from pathlib import Path
    client, app, parent, manager = queue_app
    starts, sends = AsyncMock(), AsyncMock()
    monkeypatch.setattr(Instance, 'start', starts)
    monkeypatch.setattr(Instance, 'send', sends)
    root = 'c' * 32
    workspace = Path(parent.path) / 'state/queue-work/test/attempts' / root
    workspace.mkdir(parents=True)
    headers = {'Authorization': 'Bearer ' + internal_token()}
    body = {'task_id': 1, 'workflow_id': 'pilot', 'workspace': str(workspace),
            'root_attempt': root, 'role': 'reviewer', 'round': 1, 'prompt': 'Review evidence',
            'execution': {'provider': 'codex', 'model': 'test', 'permission': 'read-only'}}
    for turn in ['d'*32, 'e'*32]:
        url = f'/api/task-queues/queue/attempts/{turn}'
        response = client.post(url, json=body, headers=headers)
        assert response.status_code == 200, response.text
        assert client.post(url, json=body, headers=headers).json() == response.json()
        child = app.state.registry.get('queue_'+turn)
        assert child.queue_attempt['root_attempt'] == root
        assert child.permission_mode == 'read-only'
        assert child.display_title.endswith('reviewer r1')
    assert starts.await_count == 2
    assert client.delete(f'/api/task-queues/queue/attempts/{root}', headers=headers).status_code == 200
    assert client.post('/api/task-queues/queue/attempts/'+'f'*32, json=body, headers=headers).status_code == 409


def test_round_history_preserves_long_events_and_restricts_attempt(queue_app, monkeypatch):
    from agent_manager.orchestration.controllers import attempt_token
    client, app, parent, manager = queue_app
    root = 'a'*32
    sender = Instance(title='review', path=parent.path, parent=parent.title, queue_profile='test', queue_id='test',
                      queue_attempt={'id':'b'*32,'root_attempt':root})
    target = Instance(title='worker', path=parent.path, parent=parent.title, queue_profile='test', queue_id='test',
                      queue_attempt={'id':'c'*32,'root_attempt':root})
    other = Instance(title='other', path=parent.path, parent=parent.title, queue_profile='test', queue_id='test',
                     queue_attempt={'id':'d'*32,'root_attempt':'e'*32})
    for inst in [sender,target,other]: app.state.registry._instances[inst.title]=inst
    monkeypatch.setattr(Instance, 'history', lambda self: [{'text':'x'*12000}] * 51)
    headers={'Authorization':'Bearer '+attempt_token('b'*32)}
    url='/api/task-queues/queue/attempts/'+'b'*32+'/history'
    page=client.post(url,headers=headers,json={'turn_id':'c'*32}).json()
    assert len(page['events'])==50 and len(page['events'][0]['text'])==12000 and page['has_more']
    page=client.post(url,headers=headers,json={'turn_id':'c'*32,'offset':50}).json()
    assert len(page['events'])==1 and not page['has_more']
    assert client.post(url,headers=headers,json={'turn_id':'d'*32}).status_code==403
    assert client.post(url,headers=headers,json={'turn_id':target.instance_id}).status_code==200
    assert client.post(url,headers=headers,json={'turn_id':other.instance_id}).status_code==403
    target.queue_attempt['previous_turns']=['f'*32]
    assert client.post(url,headers=headers,json={'turn_id':'f'*32}).status_code==200
    sender.queue_attempt['cancelled']=True
    assert client.post(url,headers=headers,json={'turn_id':'c'*32}).status_code==403


def test_conversation_abort_pauses_queue_attempt(queue_app, monkeypatch):
    import httpx
    client, app, parent, manager = queue_app
    child = Instance(title='round',path=parent.path,parent=parent.title,queue_profile='test',queue_id='test',
        queue_attempt={'id':'b'*32,'root_attempt':'a'*32,'task_id':2})
    app.state.registry._instances[child.title] = child
    manager._processes[parent.title] = SimpleNamespace(is_running=True,port=12345)
    requests = []
    real_client = httpx.AsyncClient
    def handler(request):
        requests.append(json.loads(request.content))
        return httpx.Response(200,json={'ok':True})
    monkeypatch.setattr(httpx,'AsyncClient',lambda **kwargs: real_client(transport=httpx.MockTransport(handler),**kwargs))
    response=client.post('/api/instances/round/abort')
    assert response.status_code == 200, response.text
    assert requests == [{'action':'pause_attempt','task_id':2,'attempt_id':'a'*32,'turn_id':'b'*32,'actor':'operator'}]
    manager._processes.clear()


@pytest.mark.parametrize('manifest', [None, {}])
def test_worker_launch_accepts_empty_review_manifest(queue_app, monkeypatch, manifest):
    from pathlib import Path
    client, app, parent, manager = queue_app
    monkeypatch.setattr(Instance, 'start', AsyncMock())
    send = AsyncMock()
    monkeypatch.setattr(Instance, 'send', send)
    root = 'a'*32
    turn = 'b'*32
    workspace = Path(parent.path) / 'state/queue-work/test/attempts' / root
    workspace.mkdir(parents=True)
    body = {'task_id':2, 'root_attempt':root, 'role':'worker', 'round':2,
            'workspace':str(workspace), 'prompt':'Continue with the reviewer feedback',
            'execution':{'provider':'codex','permission':'dangerFullAccess'},
            'review_files':manifest}
    response = client.post(f'/api/task-queues/queue/attempts/{turn}', json=body,
                           headers={'Authorization':'Bearer '+internal_token()})
    assert response.status_code == 200, response.text
    send.assert_awaited_once_with('Continue with the reviewer feedback')
    assert app.state.registry.get('queue_'+turn).queue_attempt['review_files'] == {}


def test_worker_continuation_reuses_session_and_delivers_feedback_once(queue_app, monkeypatch):
    from pathlib import Path
    client, app, parent, manager = queue_app
    starts, sends = AsyncMock(), AsyncMock()
    monkeypatch.setattr(Instance, 'start', starts)
    monkeypatch.setattr(Instance, 'send', sends)
    root, first, second = 'a'*32, 'b'*32, 'c'*32
    workspace=Path(parent.path)/'state/queue-work/test/attempts'/root
    workspace.mkdir(parents=True)
    headers={'Authorization':'Bearer '+internal_token()}
    body={'task_id':2,'root_attempt':root,'role':'worker','round':1,'workspace':str(workspace),
          'prompt':'Initial full task','execution':{'provider':'codex','model':'test','permission':'dangerFullAccess'},'review_files':{}}
    response=client.post(f'/api/task-queues/queue/attempts/{first}',headers=headers,json=body)
    assert response.status_code==200,response.text
    child=app.state.registry.get('queue_'+first)
    identity=child.instance_id
    child.session_id='retained-provider-thread'
    child._history=[{'type':'result','usage':{'input_tokens':900,'output_tokens':100}}]
    assert client.delete(f'/api/task-queues/queue/attempts/{first}',headers=headers).status_code==200
    feedback='Reviewer feedback: test the missing historical checkpoint.'
    continuation={**body,'round':2,'resume_worker':True,'prompt':feedback}
    url=f'/api/task-queues/queue/attempts/{second}'
    resumed=client.post(url,headers=headers,json=continuation)
    assert resumed.status_code==200,resumed.text
    assert resumed.json()['conversation_id']==identity
    assert resumed.json()['tokens']==0  # Do not count previous rounds again.
    assert child.session_id=='retained-provider-thread'
    assert child.queue_attempt['id']==second and first in child.queue_attempt['previous_turns']
    assert child.queue_attempt['history_start']>=1
    assert app.state.registry.get('queue_'+second) is None
    repeated=client.post(url,headers=headers,json=continuation)
    assert repeated.status_code==200
    assert sends.await_count==2
    assert [call.args[0] for call in sends.await_args_list]==['Initial full task',feedback]
    assert starts.await_count==2
    old=client.get(f'/api/task-queues/queue/attempts/{first}',headers=headers).json()
    assert old['exists'] and not old['busy'] and not old['alive']
    monkeypatch.setattr(manager,'_find_binary',lambda:'/tmp/am-orchestrator')
    tools=worker_mcp(child,manager)
    assert tools['queue']['args'][-1]==second
    from agent_manager.orchestration.controllers import attempt_token
    assert tools['queue']['env']['AM_ATTEMPT_TOKEN']==attempt_token(second)
    # Persisted launch reservation is sufficient for controller retries, too.
    child._task=None
    assert client.post(url,headers=headers,json=continuation).status_code==200
    assert sends.await_count==2


def test_resume_missing_worker_does_not_create_feedback_only_conversation(queue_app, monkeypatch):
    from pathlib import Path
    client, app, parent, manager=queue_app
    sends=AsyncMock();monkeypatch.setattr(Instance,'send',sends)
    root='a'*32;workspace=Path(parent.path)/'state/queue-work/test/attempts'/root;workspace.mkdir(parents=True)
    body={'task_id':2,'root_attempt':root,'role':'worker','round':2,'resume_worker':True,
          'workspace':str(workspace),'prompt':'Reviewer feedback only','execution':{'provider':'codex','permission':'dangerFullAccess'}}
    response=client.post('/api/task-queues/queue/attempts/'+'b'*32,json=body,headers={'Authorization':'Bearer '+internal_token()})
    assert response.status_code==409
    assert 'missing' in response.json()['detail']
    sends.assert_not_awaited()
    assert app.state.registry.get('queue_'+'b'*32) is None


def test_prepare_resume_retains_session_and_old_cancellation(queue_app):
    from agent_manager.orchestration.controllers import internal_token
    client, app, parent, manager = queue_app
    root, turn = 'a'*32, 'b'*32
    child = Instance(title='retained', path=parent.path, parent=parent.title,
        queue_profile='test', queue_id='test', session_id='retained-provider-session',
        queue_attempt={'id':turn,'root_attempt':root,'role':'worker','cancelled':True})
    app.state.registry._instances[child.title]=child
    headers={'Authorization':'Bearer '+internal_token()}
    url=f'/api/task-queues/queue/attempts/{root}/resume'
    assert client.post(url,json={}).status_code==401
    assert client.post(url,headers=headers,json={}).status_code==200
    assert child.session_id=='retained-provider-session'
    assert child.queue_attempt['cancelled'] is True
    child.session_id=None
    assert client.post(url,headers=headers,json={}).status_code==409
    child.session_id='retained-provider-session'
    child._inbox.put_nowait('pending')
    assert client.post(url,headers=headers,json={}).status_code==409


@pytest.mark.parametrize('managed', [True, False])
def test_normal_queue_prompt_is_routed_before_execution(queue_app, monkeypatch, managed):
    import httpx
    client, app, parent, manager = queue_app
    manager._processes[parent.title] = SimpleNamespace(is_running=True, port=12345)
    child = Instance(title='discussion', path=parent.path, parent=parent.title,
        queue_profile='test', queue_id='test', session_id='saved',
        queue_attempt={'id':'b'*32, 'root_attempt':'a'*32, 'role':'reviewer', 'task_id':7, 'cancelled':True})
    app.state.registry._instances[child.title] = child
    starts, sends = AsyncMock(), AsyncMock()
    monkeypatch.setattr(Instance, 'start', starts)
    monkeypatch.setattr(Instance, 'send', sends)
    calls = []
    real_client = httpx.AsyncClient
    def handler(request):
        calls.append(json.loads(request.content))
        sends.assert_not_awaited()
        return httpx.Response(200, json={'ok':True, 'managed':managed})
    monkeypatch.setattr(httpx, 'AsyncClient', lambda **kwargs: real_client(transport=httpx.MockTransport(handler), **kwargs))
    try:
        result = client.post('/api/instances/discussion/send', json={'text':'Please investigate the snapshot source'})
        assert result.status_code == 200, result.text
        assert result.json()['queue_managed'] is managed
        assert calls == [{'action':'conversation_prompt', 'actor':'operator', 'task_id':7,
            'attempt_id':'a'*32, 'turn_id':'b'*32, 'text':'Please investigate the snapshot source'}]
        if managed:
            starts.assert_not_awaited()
            sends.assert_not_awaited()  # Scheduler delivers after its transaction.
        else:
            starts.assert_awaited_once()
            sends.assert_awaited_once_with('Please investigate the snapshot source')
            assert child.queue_attempt['detached']
            assert worker_mcp(child, manager) == {}
            result = client.post('/api/task-queues/queue/submit', headers={'Authorization':'Bearer anything'},
                json={'attempt_id':'b'*32, 'kind':'review', 'payload':{}})
            assert result.status_code == 409
    finally:
        manager._processes.clear()


def test_rejected_queue_prompt_never_reaches_agent(queue_app, monkeypatch):
    import httpx
    client, app, parent, manager = queue_app
    manager._processes[parent.title] = SimpleNamespace(is_running=True, port=12345)
    child = Instance(title='worker', path=parent.path, parent=parent.title,
        queue_profile='test', queue_attempt={'id':'b'*32, 'root_attempt':'a'*32, 'task_id':7})
    app.state.registry._instances[child.title] = child
    sends = AsyncMock()
    monkeypatch.setattr(Instance, 'send', sends)
    real_client = httpx.AsyncClient
    monkeypatch.setattr(httpx, 'AsyncClient', lambda **kwargs: real_client(
        transport=httpx.MockTransport(lambda request: httpx.Response(409, text='Increase the token limit')), **kwargs))
    try:
        response = client.post('/api/instances/worker/send', json={'text':'Continue'})
        assert response.status_code == 409
        assert 'token limit' in response.text
        sends.assert_not_awaited()
        assert not child.queue_attempt.get('detached')
        assert any(e.get('type') == 'user_prompt_received' and e.get('text') == 'Continue' for e in child.history())
    finally:
        manager._processes.clear()


@pytest.mark.parametrize('saved_role, next_role', [('reviewer', 'reviewer'), ('', 'worker')])
def test_conversation_continuation_reuses_session_and_rotates_capability(queue_app, monkeypatch, saved_role, next_role):
    from pathlib import Path
    client, app, parent, manager = queue_app
    root, old, new = 'a'*32, 'b'*32, 'c'*32
    workspace = Path(parent.path) / 'state/queue-work/test/attempts' / root
    workspace.mkdir(parents=True)
    child = Instance(title='original-reviewer', path=str(workspace), parent=parent.title,
        provider='codex', model='test-model', permission_mode='danger-full-access',
        queue_profile='test', queue_id='test', session_id='reviewer-session',
        queue_attempt={'id':old, 'root_attempt':root, 'task_id':7, 'role':saved_role, 'cancelled':True})
    app.state.registry._instances[child.title] = child
    starts, sends = AsyncMock(), AsyncMock()
    monkeypatch.setattr(Instance, 'start', starts)
    monkeypatch.setattr(Instance, 'send', sends)
    body = {'task_id':7, 'root_attempt':root, 'role':next_role, 'round':2, 'resume_turn':old,
        'workspace':str(workspace), 'prompt':'Reconsider the blocker using this source',
        'execution':{'provider':'codex', 'model':'test-model', 'permission':'dangerFullAccess'}}
    response = client.post(f'/api/task-queues/queue/attempts/{new}', json=body,
        headers={'Authorization':'Bearer '+internal_token()})
    assert response.status_code == 200, response.text
    assert response.json()['conversation_id'] == child.instance_id
    assert child.session_id == 'reviewer-session'
    assert child.queue_attempt['id'] == new
    assert child.queue_attempt['previous_turns'] == [old]
    assert not child.queue_attempt.get('cancelled')
    sends.assert_awaited_once_with(body['prompt'])
