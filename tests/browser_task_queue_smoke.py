"""Optional Chromium UI smoke test; uses mock API responses, never starts workers.
Run: python tests/browser_task_queue_smoke.py (requires playwright + Chromium).
"""
import functools
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import threading
from urllib.parse import urlparse, parse_qs

from playwright.sync_api import sync_playwright, expect

ROOT = Path(__file__).resolve().parents[1]
QUEUE = dict(title='sample-queue', display_title='Sample queue', kind='loop', instance_type='loop',
             controller_mode='task_queue', provider='codex', status='ready', path='/workspace/sample',
             children=[], queue_profile='sample')
TEAM = dict(title='sample-team', display_title='Sample team', kind='loop', instance_type='loop',
            controller_mode='team', provider='claude', status='ready', path='/workspace/sample', children=[])


def main():
    class Handler(SimpleHTTPRequestHandler):
        def log_message(self, *args):
            pass
    server = ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(Handler, directory=str(ROOT / 'static')))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    limits = {'max_workers': 1, 'paused': False, 'available_tasks': 17, 'active_tasks': 1, 'active_workers': 1}
    errors, controls = [], []
    try:
        with sync_playwright() as p:
            browser = p.chromium.launch(headless=True, args=['--no-sandbox'])
            page = browser.new_page(viewport={'width': 1440, 'height': 1000})
            page.on('pageerror', lambda error: errors.append(str(error)))
            def route(request):
                url = urlparse(request.request.url)
                path = url.path
                if path == '/api/instances':
                    result = [QUEUE, TEAM]
                elif path == '/api/task-queue-profiles':
                    result = [{'name': 'sample', 'queue_id': 'sample', 'max_workers_ceiling': 8}]
                elif path.endswith('/task-queues/sample-queue/status'):
                    result = {'state': 'running', 'queue': limits, 'max_workers_ceiling': 8}
                elif path.endswith('/task-queues/sample-queue/tasks'):
                    result = [dict(id=42, task_type='report', status='awaiting_review', attempt_count=1,
                        max_attempts=3, latest_attempt='abc', definition_ref={'commit': 'a'*40}, parameters={'topic': 'README'},
                        latest_result={'summary': 'Report ready', 'artifacts': []})]
                elif path.endswith('/task-queues/sample-queue/logs'):
                    after = int(parse_qs(url.query).get('after', ['0'])[0])
                    result = [] if after else [dict(id=1, task_id=42, actor='worker', event_type='progress',
                        summary='Inspected the README', created_at='2026-09-11T12:00:00Z')]
                elif path.endswith('/task-queues/sample-queue/control'):
                    body = request.request.post_data_json
                    controls.append(body)
                    if body['action'] == 'max_workers':
                        limits['max_workers'] = body['max_workers']
                    result = {'ok': True}
                elif path == '/api/providers':
                    result = [{'provider':'claude','enabled':True,'label':'Claude','runtime_options':{}}]
                elif path.endswith('/models'):
                    result = []
                elif 'auth' in path:
                    result = {'authed': True}
                elif path.endswith('/children') or path == '/api/folders':
                    result = []
                elif path.endswith('/orchestrator/status'):
                    result = {'running': False, 'state': 'stopped'}
                else:
                    result = {}
                request.fulfill(status=200, content_type='application/json', body=json.dumps(result))
            page.route('**/api/**', route)
            page.goto(f'http://127.0.0.1:{server.server_port}/')
            page.wait_for_function("document.querySelector('am-app')?.instances.length === 2")
            page.evaluate('(inst) => document.querySelector("am-app").selectInstance(inst)', QUEUE)
            panel = page.locator('am-task-queue-panel')
            expect(panel).to_be_visible()
            expect(page.locator('am-task-queue-tasks')).to_be_visible()
            expect(page.locator('am-loop-pane')).not_to_be_visible()
            expect(panel.locator('[data-count=available_tasks]')).to_have_text('17')
            expect(panel.locator('[data-count=workers]')).to_have_text('1 / 1')
            panel.locator('input').fill('2')
            panel.get_by_role('button', name='Apply', exact=True).click()
            expect(panel.locator('.queue-effective')).to_have_text('Effective limit: 2')
            panel.locator('input').fill('3')
            page.evaluate('document.querySelector("am-task-queue-panel").load()')
            expect(panel.locator('input')).to_have_value('3')
            page.locator('.queue-task-rows summary').click()
            page.locator('am-task-queue-tasks').get_by_role('button', name='approve', exact=True).click()
            assert any(c['action']=='approve' and c['attempt_id']=='abc' for c in controls)
            page.get_by_role('button', name='Activity', exact=True).click()
            expect(page.locator('am-task-queue-tasks .loop-progress')).to_be_visible()
            expect(page.locator('am-task-queue-tasks .loop-actor')).to_have_text('worker → Task 42')
            page.get_by_role('button', name='Filter', exact=True).click()
            page.locator('am-toolbar input[data-type=progress]').uncheck()
            expect(page.locator('am-task-queue-tasks .loop-progress')).not_to_be_visible()
            page.get_by_role('button', name='Filter', exact=True).click()
            page.screenshot(path='/tmp/task-queue-ui.png', full_page=True)
            # The extracted team view still renders through the original conversation tab.
            page.evaluate('(inst) => document.querySelector("am-app").selectInstance(inst)', TEAM)
            expect(page.locator('am-loop-pane')).to_be_visible()
            expect(panel).not_to_be_visible()
            page.evaluate('document.querySelector("am-new-dialog").open("task_queue")')
            expect(page.locator('am-task-queue-create')).to_be_visible()
            expect(page.locator('am-task-queue-create input[name=max]')).to_have_value('1')
            expect(page.locator('am-task-queue-create select')).to_have_value('sample')
            page.evaluate('document.querySelector("am-new-dialog").setMode("team")')
            expect(page.locator('#team-yaml')).to_have_value(__import__('re').compile('gpt-6-astra'))
            assert not errors, errors
            browser.close()
        print('Browser smoke passed: modules, queue/team routing, counts, controls, filters, review, creation defaults.')
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


if __name__ == '__main__':
    main()
