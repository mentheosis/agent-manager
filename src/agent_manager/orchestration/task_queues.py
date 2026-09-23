"""HTTP/conversation adapter only. SQL scheduling and ownership live in Go."""
from __future__ import annotations

import asyncio
import hashlib
import hmac
from pathlib import Path
import re

import httpx
from fastapi import HTTPException, Request
from fastapi.responses import FileResponse

from ..instance import Instance
from .controllers import internal_token, profiles, instance_queue_config, normalize_worker


def mount_routes(app, registry, manager):
    launch_lock = asyncio.Lock()
    drains = {}

    async def drain(title):
        try:
            while True:
                current = await proxy(title, "status")
                if not current["queue"]["paused"]:
                    return
                if not current["queue"]["active_workers"]:
                    await manager.stop(title)
                    return
                await asyncio.sleep(2)
        except HTTPException:
            pass
        finally:
            drains.pop(title, None)

    def parent(title):
        inst = registry.get(title)
        if not inst:
            raise HTTPException(404, 'Queue not found')
        if inst.controller_mode != 'task_queue':
            raise HTTPException(400, 'Not a task queue')
        return inst

    def authenticate(request):
        token = request.headers.get('authorization', '').removeprefix('Bearer ')
        if not hmac.compare_digest(token, internal_token()):
            raise HTTPException(401, 'Invalid controller capability')

    async def proxy(title, path, method='GET', body=None, token=None):
        parent(title)
        proc = manager.get(title)
        if not proc or not proc.is_running:
            raise HTTPException(409, 'Start the queue controller first')
        try:
            async with httpx.AsyncClient(timeout=30, trust_env=False) as client:
                response = await client.request(method, f'http://127.0.0.1:{proc.port}/{path}',
                    json=body, headers={'Authorization': 'Bearer ' + (token or internal_token())})
                if response.status_code >= 400:
                    # Go errors are configuration/state messages, never credentials.
                    raise HTTPException(response.status_code, response.text[:2000])
                return response.json()
        except httpx.HTTPError as exc:
            raise HTTPException(503, 'Queue controller unavailable') from exc

    async def send_conversation(child, text, images=None):
        # SQL/controller decides eligibility before any provider sees the prompt.
        # Keep old capabilities fenced even when an old conversation is reopened.
        if images:
            raise HTTPException(400, 'Queue conversation continuations currently accept text only')
        # Persist before classification, runtime startup, or inbox delivery.
        await child._publish({'type': 'user_prompt_received', 'text': text})
        metadata = child.queue_attempt
        result = await proxy(child.parent, 'control', 'POST', {
            'action': 'conversation_prompt', 'actor': 'operator',
            'task_id': metadata['task_id'],
            'attempt_id': metadata.get('root_attempt') or metadata['id'],
            'turn_id': metadata['id'], 'text': text,
        })
        if result.get('managed'):
            return {'ok': True, 'queue_managed': True}
        async with launch_lock:
            if child.status == 'running' or not child._inbox.empty():
                raise HTTPException(409, 'This conversation is already running')
            await child.stop()
            metadata['detached'] = True
            await registry._save_records()
            await child.start()
            await child.send(text)
        return {'ok': True, 'queue_managed': False}

    app.state.send_queue_conversation = send_conversation

    async def delete_controller(title):
        inst = parent(title)
        pending = drains.pop(title, None)
        if pending:
            pending.cancel()
        proc = manager.get(title)
        if proc and proc.is_running:
            await proxy(title, 'control', 'POST', {'action': 'pause', 'actor': 'operator'})
            await proxy(title, 'control', 'POST', {'action': 'cancel_all', 'actor': 'operator'})
        await manager.stop(title)
        # Keep attempt conversations discoverable by immutable correlation IDs.
        # If the Go process was already down, expired SQL attempts are reconciled
        # by a subsequent controller after these local workers have been stopped.
        async with launch_lock:
            for child in registry.list():
                if child.parent == title and child.queue_attempt:
                    tombstone = cancellation_path(inst, child.queue_attempt['id'])
                    tombstone.parent.mkdir(parents=True, exist_ok=True)
                    tombstone.touch(exist_ok=True)
                    await child.stop()
                    child.queue_attempt['cancelled'] = True
                    child.parent = None
            inst.children.clear()
            await registry.delete(title, cascade=False)

    app.state.delete_queue_controller = delete_controller

    async def cancel_queue_conversation(child):
        if not child.parent:
            raise HTTPException(409, 'Queue controller is unavailable; open its queue controls')
        await proxy(child.parent, 'control', 'POST', {
            'action': 'pause_attempt', 'task_id': child.queue_attempt['task_id'],
            'turn_id': child.queue_attempt['id'],
            'attempt_id': child.queue_attempt.get('root_attempt') or child.queue_attempt['id'],
            'actor': 'operator'})
        return {'ok': True}

    app.state.cancel_queue_conversation = cancel_queue_conversation

    @app.get('/api/task-queue-profiles')
    async def list_profiles():
        try:
            return [{'name': name,
                     'default_max_workers': p.get('default_max_workers', 1),
                     'default_lease_secs': p.get('default_lease_secs', 60),
                     'default_task_limit_secs': p.get('default_task_limit_secs', 3600),
                     'default_task_limit_tokens': p.get('default_task_limit_tokens', 2000000),
                     'default_review_round_limit': p.get('default_review_round_limit', 8),
                     'tasks': list(p.get('tasks', {}))}
                    for name, p in profiles().items()]
        except (ValueError, OSError):
            raise HTTPException(500, 'Task queue profiles are invalid or unavailable')

    @app.post('/api/task-queues/{title}/render')
    async def render_batch(title: str, request: Request):
        return await load_batch(title, request, 'render')

    @app.post('/api/task-queues/{title}/enqueue')
    async def enqueue_batch(title: str, request: Request):
        return await load_batch(title, request, 'enqueue')

    async def load_batch(title, request, action):
        import json
        from .task_loading import queue_command
        inst = parent(title)
        raw = await request.body()
        if len(raw) > 1 << 20:
            raise HTTPException(413, 'Batch exceeds 1 MiB')
        try:
            body = json.loads(raw)
            if not isinstance(body, dict):
                raise ValueError('Batch must be an object')
            if action == 'enqueue' and not body.get('preview_hash'):
                raise ValueError('Preview the batch before loading')
            return await queue_command(instance_queue_config(inst), manager.find_binary(), action, body)
        except (ValueError, OSError) as exc:
            raise HTTPException(400, str(exc))
        except asyncio.TimeoutError:
            raise HTTPException(504, 'Queue command timed out; retry the same batch key to reconcile')

    @app.post('/api/task-queues/{title}/start')
    async def start(title: str):
        inst = parent(title)
        try:
            instance_queue_config(inst)
            proc = manager.get(title)
            if not proc or not proc.is_running:
                proc = await manager.start(inst)
            return {'ok': True, 'pid': proc.pid, 'port': proc.port}
        except (ValueError, OSError, RuntimeError) as exc:
            raise HTTPException(400, str(exc))

    @app.get('/api/task-queues/{title}/status')
    async def status(title: str):
        inst = parent(title)
        proc = manager.get(title)
        if not proc or not proc.is_running:
            profile = profiles()[inst.queue_profile or inst.queue_id]
            def setting(field, key, default):
                value = getattr(inst, field, None)
                return value if value is not None else profile.get(key, default)
            return {'state': 'stopped', 'queue': None, 'settings': {
                'queue_id': inst.queue_profile or inst.queue_id,
                'max_workers': setting('queue_initial_max_workers', 'default_max_workers', 1),
                'limits': {
                    'lease_secs': setting('queue_lease_secs', 'default_lease_secs', 60),
                    'task_limit_secs': setting('queue_task_limit_secs', 'default_task_limit_secs', 3600),
                    'review_round_limit': setting('queue_review_round_limit', 'default_review_round_limit', 8),
                    'task_limit_tokens': setting('queue_task_limit_tokens', 'default_task_limit_tokens', 2000000)}}}
        result = await proxy(title, 'status')
        return {**result, 'draining': title in drains}

    async def read_queue(title, action, payload):
        from .task_loading import queue_command
        inst = parent(title)
        try:
            return await queue_command(instance_queue_config(inst), manager.find_binary(), action, payload)
        except (ValueError, OSError) as exc:
            raise HTTPException(400, str(exc))
        except asyncio.TimeoutError:
            raise HTTPException(504, 'Queue read timed out')

    @app.get('/api/task-queues/{title}/tasks')
    async def tasks(title: str, offset: int = 0):
        proc = manager.get(title)
        if not proc or not proc.is_running:
            return await read_queue(title, 'tasks', {'offset': max(0, offset)})
        return await proxy(title, f'tasks?offset={max(0, offset)}')

    @app.get('/api/task-queues/{title}/logs')
    async def logs(title: str, after: int = 0):
        proc = manager.get(title)
        if not proc or not proc.is_running:
            return await read_queue(title, 'logs', {'after': max(0, after)})
        return await proxy(title, f'logs?after={max(0, after)}')

    @app.post('/api/task-queues/{title}/control')
    async def control(title: str, body: dict):
        body = {**body, 'actor': 'operator'}
        if body.get('action') == 'resume' and title in drains:
            drains.pop(title).cancel()
        if body.get('action') == 'stop':
            # Stop = pause dispatch + drain. Keep supervision alive until every
            # running attempt has ended; SQL ownership must not be abandoned.
            await proxy(title, 'control', 'POST', {'action': 'pause', 'actor': 'operator'})
            current = await proxy(title, 'status')
            if current['queue']['active_workers']:
                if title not in drains:
                    drains[title] = asyncio.create_task(drain(title), name=f'queue-drain:{title}')
                return {'ok': True, 'draining': True}
            await manager.stop(title)
            return {'ok': True}
        return await proxy(title, 'control', 'POST', body)

    @app.get('/api/task-queues/{title}/artifacts/{digest}')
    async def artifact(title: str, digest: str):
        inst = parent(title)
        if not re.fullmatch(r'[a-f0-9]{64}', digest):
            raise HTTPException(404, 'Artifact not found')
        cfg = instance_queue_config(inst)
        root = (Path(cfg['workspace_root']) / 'artifacts').resolve()
        file = (root / digest).resolve()
        if file.parent != root or not file.is_file():
            raise HTTPException(404, 'Artifact not found')
        return FileResponse(file, media_type='application/octet-stream', filename=digest,
                            headers={'X-Content-Type-Options': 'nosniff'})

    @app.post('/api/task-queues/{title}/submit')
    async def submit(title: str, request: Request):
        data = await request.body()
        if len(data) > 1 << 20:
            raise HTTPException(413, 'Submission too large')
        try:
            import json
            body = json.loads(data)
        except ValueError:
            raise HTTPException(400, 'Invalid JSON')
        token = request.headers.get('authorization', '').removeprefix('Bearer ')
        if not token:
            raise HTTPException(401, 'Attempt capability required')
        child = worker(title, body.get('attempt_id'))
        if child and child.queue_attempt.get('detached'):
            raise HTTPException(409, 'This conversation is outside the task queue')
        return await proxy(title, 'submit', 'POST', body, token)

    def worker(title, attempt):
        for inst in registry.list():
            if inst.queue_attempt and (inst.queue_attempt['id'] == attempt or attempt in inst.queue_attempt.get('previous_turns', [])):
                expected = instance_queue_config(parent(title))
                actual = instance_queue_config(inst)
                if any(expected.get(k) != actual.get(k) for k in ('dsn', 'queue_id', 'workspace_root')):
                    raise HTTPException(409, 'Attempt belongs to a different queue')
                return inst
        return None

    def summary(inst):
        if inst is None:
            return {'exists': False, 'busy': False, 'alive': False, 'launched': False}
        tokens, cost = 0, 0.0
        for event in inst.history()[inst.queue_attempt.get('history_start', 0):]:
            data = event.get('diagnostics') or event
            usage = data.get('usage') or {}
            if isinstance(usage, dict):
                tokens = max(tokens, int(usage.get('total_tokens') or
                    sum(usage.get(k) or 0 for k in ('input_tokens', 'output_tokens', 'cache_creation_input_tokens', 'cache_read_input_tokens'))))
            cost = max(cost, float(data.get('total_cost_usd') or 0))
        return {'exists': True, 'tokens': tokens, 'cost_usd': cost, 'conversation_id': inst.instance_id,
                'busy': inst.status in ('creating', 'running') or not inst._inbox.empty(),
                'alive': inst._task is not None and not inst._task.done(),
                'launched': inst.queue_attempt.get('launch_state') == 'reserved'}

    def cancellation_path(inst, attempt):
        if not re.fullmatch(r'[a-f0-9]{32}', attempt):
            raise HTTPException(400, 'Invalid attempt ID')
        cfg = instance_queue_config(inst)
        return Path(cfg['workspace_root']) / 'cancelled-attempts' / attempt

    @app.get('/api/task-queues/{title}/attempts/{attempt}')
    async def attempt_state(title: str, attempt: str, request: Request):
        authenticate(request)
        parent(title)
        child = worker(title, attempt)
        if child and (child.queue_attempt['id'] != attempt or child.queue_attempt.get('detached')):
            return {'exists': True, 'busy': False, 'alive': False, 'launched': True, 'conversation_id': child.instance_id}
        return summary(child)

    @app.post('/api/task-queues/{title}/attempts/{attempt}')
    async def launch(title: str, attempt: str, request: Request):
        authenticate(request)
        inst = parent(title)
        raw = await request.body()
        if len(raw) > 4 << 20:
            raise HTTPException(413, 'Launch payload too large')
        import json
        body = json.loads(raw)
        prompt = body.get('prompt', '')
        if not isinstance(prompt, str) or not prompt:
            raise HTTPException(400, 'Prompt is required')
        async with launch_lock:
            tombstone = cancellation_path(inst, attempt)
            if tombstone.exists():
                raise HTTPException(409, 'Attempt was cancelled')
            existing = worker(title, attempt)
            execution = body.get('execution')
            if not isinstance(execution, dict):
                raise HTTPException(400, 'Resolved execution settings are required')
            try:
                execution = normalize_worker(execution)
            except ValueError as exc:
                raise HTTPException(400, str(exc))
            use_isolated = body.get('use_isolated_workspace', True)
            if type(use_isolated) is not bool:
                raise HTTPException(400, 'Workspace mode must be boolean')
            workspace = Path(body.get('workspace', '')).resolve()
            digest = hashlib.sha256(json.dumps({'prompt': prompt, 'execution': execution,
                'workspace': str(workspace), 'use_isolated_workspace': use_isolated,
                'repository': body.get('repository'), 'root_attempt': body.get('root_attempt'),
                'role': body.get('role'), 'round': body.get('round'), 'resume_worker': body.get('resume_worker', False), 'resume_turn': body.get('resume_turn', ''), 'review_files': body.get('review_files', {})}, sort_keys=True).encode()).hexdigest()
            if existing:
                if existing.queue_attempt.get('prompt_hash') != digest:
                    raise HTTPException(409, 'Attempt launch input changed')
                return summary(existing)
            cfg = instance_queue_config(inst)
            root_attempt = body.get('root_attempt') or attempt
            if cancellation_path(inst, root_attempt).exists():
                raise HTTPException(409, 'Root attempt was cancelled')
            if not re.fullmatch(r'[a-f0-9]{32}', root_attempt):
                raise HTTPException(400, 'Invalid root attempt')
            attempt_root = Path(cfg['workspace_root']).resolve() / 'attempts' / root_attempt
            add_dirs = []
            if use_isolated:
                expected = attempt_root
            else:
                # The authenticated scheduler supplies the saved mode/repository.
                # A retry may retain shared mode after the profile default changes.
                source = cfg.get('repositories', {}).get(body.get('repository'))
                if not source or not Path(source).is_absolute():
                    raise HTTPException(400, 'Shared workspace must name an approved repository')
                expected = Path(source).resolve()
                inputs = attempt_root / '.queue-inputs'
                if not inputs.is_dir() or inputs.resolve() != inputs or attempt_root.is_relative_to(expected):
                    raise HTTPException(400, 'Invalid attempt evidence directory')
                add_dirs = [str(inputs)]
            if workspace != expected or not workspace.is_dir():
                raise HTTPException(400, 'Workspace does not match attempt')
            review_files = body.get('review_files')
            if review_files is None:
                review_files = {}
            if not isinstance(review_files, dict) or len(review_files) > 100:
                raise HTTPException(400, 'Invalid review file manifest')
            for label, location in review_files.items():
                if not isinstance(label, str) or not isinstance(location, str):
                    raise HTTPException(400, 'Invalid review file entry')
                resolved = Path(location).resolve()
                if not (resolved.is_relative_to(workspace) or resolved.is_relative_to(attempt_root / '.queue-inputs')) or not resolved.is_file():
                    raise HTTPException(400, 'Review file is outside approved workspace/evidence')
                review_files[label] = str(resolved)
            name = 'queue_' + attempt
            if registry.get(name):
                raise HTTPException(409, 'Conversation name collision')
            workflow = body.get('workflow_id') or cfg['queue_id']
            attempt_number = body.get('attempt_number', 1)
            role_suffix = f" {body['role']} r{body.get('round', 1)}" if body.get('role') else ''
            metadata = {'id': attempt, 'root_attempt': root_attempt, 'role': body.get('role'),
                        'round': body.get('round'), 'review_files': review_files,
                        'task_id': body['task_id'], 'launch_state': 'reserved', 'prompt_hash': digest,
                        'replay_profile': body.get('execution', {}).get('replay_profile')}
            display = f"{workflow}-{body['task_id']}-{attempt_number} {root_attempt[:8]}{role_suffix}"
            if body.get('resume_worker') or body.get('resume_turn'):
                if body.get('role') not in ('worker', 'reviewer'):
                    raise HTTPException(400, 'Only a worker or reviewer conversation can resume')
                candidates = [candidate for candidate in registry.list() if candidate.queue_attempt
                              and candidate.queue_attempt.get('root_attempt') == root_attempt
                              and (candidate.queue_attempt.get('role') or 'worker') == body.get('role')
                              and (not body.get('resume_turn') or candidate.queue_attempt['id'] == body['resume_turn'])]
                if len(candidates) != 1:
                    raise HTTPException(409, 'Original conversation is missing or ambiguous; continuation was not delivered')
                child = candidates[0]
                worker(title, child.queue_attempt['id'])  # Verify queue ownership.
                if not child.session_id:
                    raise HTTPException(409, 'Conversation has no retained provider session; cannot resume feedback alone')
                if child._task and not child._task.done() or not child._inbox.empty():
                    raise HTTPException(409, 'Previous conversation turn has not stopped')
                if (child.path, child.provider, child.model, child.permission_mode) != (
                        str(workspace), execution['provider'], execution['model'], execution['permission_mode']):
                    raise HTTPException(409, 'Conversation execution settings changed')
                old = child.queue_attempt
                metadata['previous_turns'] = [*old.get('previous_turns', []), old['id']]
                metadata['replay_jobs'] = old.get('replay_jobs', [])
                metadata['history_start'] = len(child.history())
                child.queue_attempt = metadata
                child.display_title = display
                child.add_dirs = add_dirs
                if child.parent != title:
                    prior_parent = registry.get(child.parent) if child.parent else None
                    if prior_parent and child.title in prior_parent.children:
                        prior_parent.children.remove(child.title)
                    child.parent = title
                    if child.title not in inst.children:
                        inst.children.append(child.title)
            else:
                child = Instance(title=name, display_title=display,
                    path=str(workspace), provider=execution['provider'], model=execution['model'],
                    permission_mode=execution['permission_mode'], add_dirs=add_dirs,
                    parent=title, queue_profile=inst.queue_profile, queue_id=cfg['queue_id'], queue_attempt=metadata)
                registry._wire_hooks(child)
                registry._instances[name] = child
                inst.children.append(name)
            # Persist reservation before launching. An ambiguous crash cannot cause
            # this attempt to be dispatched twice; the scheduler retries a new attempt.
            await registry._save_records()
            await child.start()
            await child.send(prompt)
            return summary(child)

    @app.post('/api/task-queues/{title}/attempts/{attempt}/history')
    async def attempt_history(title: str, attempt: str, request: Request):
        from .controllers import attempt_token
        token = request.headers.get('authorization', '').removeprefix('Bearer ')
        if not hmac.compare_digest(token, attempt_token(attempt)):
            raise HTTPException(401, 'Invalid turn capability')
        child = worker(title, attempt)
        if not child or child.queue_attempt['id'] != attempt or (child.queue_attempt.get('cancelled') or child.queue_attempt.get('detached')):
            raise HTTPException(403, 'Turn is not active')
        body = await request.json()
        target_id = body.get('turn_id', attempt)
        target = worker(title, target_id)
        if target is None:
            # Review history exposes both round IDs and conversation IDs. Resolve
            # either, then apply the same queue ownership and attempt checks.
            candidate = next((item for item in registry.list()
                              if item.instance_id == target_id and item.queue_attempt), None)
            if candidate is not None:
                target = worker(title, candidate.queue_attempt['id'])
        root = child.queue_attempt.get('root_attempt') or attempt
        if not target or (target.queue_attempt.get('root_attempt') or target.queue_attempt['id']) != root:
            raise HTTPException(403, 'History must belong to the same attempt')
        offset = body.get('offset', 0)
        if type(offset) is not int or offset < 0:
            raise HTTPException(400, 'offset must be nonnegative')
        events = target.history()
        page = events[offset:offset + 50]
        return {'events': page, 'next_offset': offset + len(page), 'has_more': offset + len(page) < len(events)}

    @app.post('/api/task-queues/{title}/attempts/{attempt}/replay')
    async def replay_attempt(title: str, attempt: str, request: Request):
        import os, json
        from .controllers import attempt_token
        from .task_loading import queue_command
        token = request.headers.get('authorization', '').removeprefix('Bearer ')
        if not hmac.compare_digest(token, attempt_token(attempt)):
            raise HTTPException(401, 'Invalid attempt capability')
        child = worker(title, attempt)
        if not child or child.queue_attempt['id'] != attempt or (child.queue_attempt.get('cancelled') or child.queue_attempt.get('detached')) or not child.queue_attempt.get('replay_profile'):
            raise HTTPException(403, 'Replay is not enabled for this attempt')
        cfg = instance_queue_config(child)
        state = await queue_command(cfg, manager.find_binary(), 'attempt-status', {'attempt': attempt})
        if state.get('status') not in ('claimed', 'running'):
            raise HTTPException(409, 'Attempt is no longer active')
        raw = await request.body()
        if len(raw)>20000: raise HTTPException(413, 'Replay request too large')
        body = json.loads(raw)
        if not isinstance(body, dict) or set(body)-{'action','run_key','operation','sql','job_id'}:
            raise HTTPException(400, 'Invalid replay arguments')
        action=body.get('action')
        jobs=child.queue_attempt.setdefault('replay_jobs', [])
        if action in ('job_status','job_logs'):
            if body.get('job_id') not in jobs: raise HTTPException(403, 'Job does not belong to this attempt')
            tool='get_job_status' if action=='job_status' else 'tail_job_log'
            args={'job_id':body['job_id']}
            if action=='job_logs': args['max_lines']=200
        elif action in ('start','status','logs','cancel','query'):
            tool='compose'
            args={k:v for k,v in body.items() if k!='job_id'}
            args['profile']=child.queue_attempt['replay_profile']
            if action!='query':
                suffix=body.get('run_key','')
                if not isinstance(suffix,str) or not re.fullmatch(r'[A-Za-z0-9_-]{1,40}',suffix): raise HTTPException(400, 'run_key must be a 1..40 character scenario-step suffix')
                scope=hashlib.sha256((cfg['queue_id']+':'+str(child.queue_attempt['task_id'])).encode()).hexdigest()[:20]
                args['run_key']=scope+'-'+suffix
        else: raise HTTPException(400, 'Invalid replay action')
        url=os.environ.get('DOCKER_MCP_URL');secret=os.environ.get('DOCKER_MCP_TOKEN')
        if not url or not secret: raise HTTPException(503, 'Host MCP is not configured on the backend')
        try:
            async with httpx.AsyncClient(timeout=30,trust_env=False) as client:
                response=await client.post(url,headers={'Authorization':'Bearer '+secret},json={'jsonrpc':'2.0','id':1,'method':'tools/call','params':{'name':tool,'arguments':args}})
                response.raise_for_status();result=response.json()
            if tool=='compose':
                for content in result.get('result',{}).get('content',[]):
                    if content.get('type')=='text':
                        try: job=json.loads(content['text'])
                        except (ValueError,KeyError): continue
                        if isinstance(job,dict) and job.get('id'):
                            jobs.append(job['id']);await registry._save_records()
            return result
        except (httpx.HTTPError, ValueError):
            raise HTTPException(503, 'Host replay request unavailable; inspect the same run key before retrying')

    @app.post('/api/task-queues/{title}/attempts/{attempt}/resume')
    async def prepare_attempt_resume(title: str, attempt: str, request: Request):
        authenticate(request)
        inst = parent(title)
        body = await request.json()
        async with launch_lock:
            children = [child for child in registry.list() if child.queue_attempt
                        and child.queue_attempt.get('root_attempt') == attempt]
            workers = [child for child in children if (child.queue_attempt.get('role') or 'worker') == 'worker']
            if len(workers) != 1 or not workers[0].session_id:
                raise HTTPException(409, 'Original worker session is unavailable; Resume cannot create a replacement')
            if body.get('turn_id'):
                target = next((child for child in children if child.queue_attempt['id'] == body['turn_id']), None)
                if not target or not target.session_id:
                    raise HTTPException(409, 'Original conversation session is unavailable')
            for child in children:
                worker(title, child.queue_attempt['id'])
                if child.queue_attempt.get('detached') and child.status == 'ready' and child._inbox.empty():
                    await child.stop()
                if (child._task and not child._task.done()) or not child._inbox.empty():
                    raise HTTPException(409, 'An attempt conversation is still active')
            # Old rounds remain cancelled. Only a newly allocated round may launch.
            cancellation_path(inst, attempt).unlink(missing_ok=True)
        return {'ok': True}

    @app.delete('/api/task-queues/{title}/attempts/{attempt}')
    async def cancel_attempt(title: str, attempt: str, request: Request):
        authenticate(request)
        inst = parent(title)
        async with launch_lock:
            tombstone = cancellation_path(inst, attempt)
            tombstone.parent.mkdir(parents=True, exist_ok=True)
            tombstone.touch(exist_ok=True)
            children = [child for child in registry.list() if child.queue_attempt and
                        (child.queue_attempt['id'] == attempt or child.queue_attempt.get('root_attempt') == attempt)
                        and not child.queue_attempt.get('detached')]
            for child in children:
                worker(title, child.queue_attempt['id'])  # Verify queue ownership.
                await child.stop()
                while not child._inbox.empty():
                    child._inbox.get_nowait()
                child.queue_attempt['cancelled'] = True
            await registry._save_records()
        return {'ok': True}
