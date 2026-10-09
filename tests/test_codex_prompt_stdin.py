import asyncio
import hashlib
import json
import sys
import tempfile
import unittest
from unittest.mock import AsyncMock, patch

from agent_manager.providers.base import AgentConfig, AgentInput
from agent_manager.providers.codex import CodexRuntime


class PromptStdinTests(unittest.IsolatedAsyncioTestCase):
    async def test_large_unicode_prompt_reaches_fresh_and_resumed_child_intact(self):
        prompt = 'snapshot review α\n' * 30000
        for session in (None, 'existing-session'):
            with self.subTest(session=session), tempfile.TemporaryDirectory() as cwd:
                runtime = CodexRuntime(AgentConfig(title='test', provider='codex', cwd=cwd, session_id=session))
                expected = runtime._prompt_with_context(prompt).encode('utf-8')
                spawn = asyncio.create_subprocess_exec
                async def child(*args, **kwargs):
                    self.assertEqual(args[-1], '-')
                    self.assertNotIn(prompt, args)
                    if session:
                        self.assertEqual(args[-2], session)
                    self.assertIn('stdin', kwargs)
                    script = "import sys,json,hashlib; b=sys.stdin.buffer.read(); print(json.dumps({'type':'item.completed','item':{'type':'agent_message','text':hashlib.sha256(b).hexdigest()}}))"
                    return await spawn(sys.executable, '-c', script, **kwargs)
                with patch('agent_manager.providers.codex.asyncio.create_subprocess_exec', side_effect=child), \
                     patch.object(runtime, '_system_init_context', new=AsyncMock(return_value={})), \
                     patch.object(runtime, '_latest_transcript_diagnostics', return_value=None), \
                     patch.object(runtime, '_codex_session_path', return_value=None), \
                     patch.object(runtime, '_start_transcript_tail', return_value=None), \
                     patch.object(runtime, '_generated_image_paths', return_value=set()), \
                     patch.object(runtime, '_new_generated_image_events', return_value=[]):
                    events = [event async for event in runtime._produce_events(AgentInput(text=prompt))]
                self.assertTrue(any(e.get('text') == hashlib.sha256(expected).hexdigest() for e in events), events)
                self.assertFalse(any(e.get('type') == 'error' for e in events), events)
