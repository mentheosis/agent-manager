import asyncio
from unittest.mock import AsyncMock

from agent_manager.instance import Instance
from agent_manager.state import Registry


async def test_loop_never_constructs_provider_runtime():
    inst = Instance(title='team', path='/tmp', kind='loop')
    def fail(config):
        raise AssertionError('team constructed a provider runtime')
    inst._runtime_factory = fail
    await inst.start()
    await inst._task
    assert inst.status == 'ready'
    assert inst._runtime is None
    assert not any(e['type'] == 'system_init' for e in inst.history())


async def test_convert_legacy_parent_closes_runtime():
    registry = Registry()
    inst = Instance(title='team', path='/tmp')
    inst.reload_options = AsyncMock()
    registry._instances['team'] = inst
    await registry.update_instance_type('team', instance_type='loop')
    inst.reload_options.assert_awaited_once()
    assert inst.kind == 'loop'


async def test_child_events_forwarded_with_independent_parent_sequence():
    registry = Registry()
    parent = Instance(title='team', path='/tmp', kind='loop')
    worker = Instance(title='worker', path='/tmp', parent='team')
    unrelated = Instance(title='other', path='/tmp')
    registry._instances.update(team=parent, worker=worker, other=unrelated)
    for inst in registry.list(): registry._wire_hooks(inst)
    await parent._publish({'type': 'team_event', 'text': 'controller started'})
    await worker._publish({'type': 'user_prompt', 'text': 'Read README'})
    await worker._publish({'type': 'assistant_text', 'text': 'Here is my summary'})
    await worker._publish({'type': 'tool_use', 'name': 'Bash', 'input': {'command': 'ls'}})
    await unrelated._publish({'type': 'assistant_text', 'text': 'Unrelated'})
    await worker._publish({'type': 'tool_use', 'name': 'mcp__team__send_to_agent', 'input': {'agent': 'reviewer', 'prompt': 'Check result'}})
    await worker._publish({'type': 'result', 'terminal': False})
    events = parent.history()
    assert [e['seq'] for e in events] == [0, 1, 2, 3]
    assert events[1]['actor'] == 'worker'
    assert events[1]['source_seq'] == 0
    assert events[2]['text'] == 'Here is my summary'
    assert events[3]['target'] == 'reviewer'
    assert worker.history()[0]['seq'] == 0
    assert all(e['type'] == 'team_event' for e in events)


async def test_controller_output_published_to_team_stream():
    from agent_manager.orchestrator import OrchestratorManager, OrchestratorProcess
    parent = Instance(title='team', path='/tmp', kind='loop')
    reader = asyncio.StreamReader()
    reader.feed_data(b'Sending initial task to orchestrator\n')
    reader.feed_eof()
    from types import SimpleNamespace
    process = SimpleNamespace(stdout=reader, wait=AsyncMock(), returncode=0)
    proc = OrchestratorProcess('team', process=process, instance=parent)
    await OrchestratorManager()._read_output(proc)
    assert parent.history()[0]['actor'] == 'Controller'
    assert parent.history()[0]['text'] == 'Sending initial task to orchestrator'
    assert 'stopped' in parent.history()[-1]['text']


async def test_legacy_team_backfill_is_chronological_and_not_repeated():
    registry = Registry()
    parent = Instance(title='team', path='/tmp', kind='loop')
    worker = Instance(title='worker', path='/tmp', parent='team')
    leader = Instance(title='leader', path='/tmp', parent='team')
    registry._instances.update(team=parent, worker=worker, leader=leader)
    await leader._publish({'type': 'user_prompt', 'text': 'Initial task', 'ts': '2026-09-10T10:00:00Z'})
    await worker._publish({'type': 'assistant_text', 'text': 'Work finished', 'ts': '2026-09-10T10:01:00Z'})
    for inst in registry.list(): registry._wire_hooks(inst)
    await registry._backfill_team_history()
    await registry._backfill_team_history()
    assert [e['text'] for e in parent.history()] == ['Initial task', 'Work finished']


async def test_forwarded_messages_are_persisted_for_replay(tmp_path):
    from agent_manager.persistence import Persistence
    persistence = Persistence(tmp_path)
    registry = Registry(persistence)
    parent = Instance(title='team', path='/tmp', kind='loop')
    worker = Instance(title='worker', path='/tmp', parent='team')
    registry._instances.update(team=parent, worker=worker)
    for inst in registry.list(): registry._wire_hooks(inst)
    await worker._publish({'type': 'assistant_text', 'text': 'Verified result'})
    events = await persistence.load_events('team')
    assert len(events) == 1
    assert events[0]['type'] == 'team_event'
    assert events[0]['actor'] == 'worker'
    assert events[0]['text'] == 'Verified result'
