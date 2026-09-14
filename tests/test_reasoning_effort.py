from unittest.mock import AsyncMock

import pytest
from fastapi.testclient import TestClient

from agent_manager.instance import Instance
from agent_manager.persistence import InstanceRecord
from agent_manager.providers.base import AgentConfig
from agent_manager.providers.codex import CodexRuntime
from agent_manager.providers import codex_metadata
from agent_manager.server import build_app


@pytest.mark.parametrize('session_id', [None, 'session-1'])
def test_effort_applied_to_fresh_and_resumed_commands(session_id):
    config = AgentConfig(title='test', provider='codex', cwd='/tmp', model='test-model', session_id=session_id)
    runtime = CodexRuntime(config)
    assert not any('model_reasoning_effort' in arg for arg in runtime._build_command('hi', []))
    runtime.set_model_options('test-model', 'high')
    command = runtime._build_command('hi', [])
    index = command.index('model_reasoning_effort="high"')
    assert command[index - 1] == '-c'
    assert config.reasoning_effort is None  # prior invocation config remains immutable
    runtime.set_model_options('test-model', None)
    assert not any('model_reasoning_effort' in arg for arg in runtime._build_command('hi', []))


def test_reasoning_persistence_and_legacy_records():
    record = InstanceRecord(title='test', path='/tmp', provider='codex', reasoning_effort='high')
    assert InstanceRecord.from_dict(record.to_dict()).reasoning_effort == 'high'
    assert InstanceRecord.from_dict({'title': 'old', 'path': '/tmp'}).reasoning_effort is None


@pytest.mark.asyncio
async def test_catalog_efforts_and_refresh(monkeypatch):
    monkeypatch.setattr(codex_metadata, '_effort_cache', None)
    fetch = AsyncMock(return_value={'models': [
        {'slug': 'test', 'visibility': 'list', 'default_reasoning_level': 'low', 'supported_reasoning_levels': [
            {'effort': 'low', 'description': 'Fast'}, {'effort': 'ultra', 'description': 'Automatic delegation'}]},
        {'slug': 'hidden', 'visibility': 'hide'},
    ]})
    monkeypatch.setattr(codex_metadata, '_run_json', fetch)
    result = await codex_metadata.fetch_codex_reasoning_options()
    assert list(result) == ['test']
    assert result['test']['levels'][1]['description'] == 'Automatic delegation'
    await codex_metadata.fetch_codex_reasoning_options()
    assert fetch.await_count == 1
    await codex_metadata.fetch_codex_reasoning_options(refresh=True)
    assert fetch.await_count == 2


def test_save_next_turn_validate_and_restore(tmp_path, monkeypatch):
    monkeypatch.setenv('AGENT_MANAGER_STATE_DIR', str(tmp_path))
    monkeypatch.setattr(Instance, 'start', AsyncMock())
    restart = AsyncMock()
    monkeypatch.setattr(Instance, 'reload_options', restart)
    monkeypatch.setattr('agent_manager.server._prefetch_provider_models', AsyncMock())
    monkeypatch.setattr('agent_manager.server.fetch_codex_reasoning_options', AsyncMock(return_value={
        'test': {'default': 'low', 'levels': [{'effort': 'low'}, {'effort': 'high'}]},
        'other': {'levels': [{'effort': 'low'}]},
    }))
    with TestClient(build_app()) as client:
        response = client.post('/api/instances', json={'name': 'effort', 'path': str(tmp_path), 'provider': 'codex', 'model': 'test'})
        assert response.status_code == 201
        title = response.json()['title']
        url = f'/api/instances/{title}/permissions'
        assert client.patch(url, json={'reasoning_effort': 'high'}).json()['reasoning_effort'] == 'high'
        restart.assert_not_awaited()
        assert client.patch(url, json={'model': 'other'}).status_code == 422
        assert client.get(f'/api/instances/{title}').json()['model'] == 'test'
    with TestClient(build_app()) as client:
        assert client.get(f'/api/instances/{title}').json()['reasoning_effort'] == 'high'
        response = client.patch(url, json={'model': 'other', 'reasoning_effort': None})
        assert response.status_code == 200
        assert response.json()['reasoning_effort'] is None
        restart.assert_not_awaited()
