import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from agent_manager.providers.codex_tool_identity import enrich_mcp_history


class ToolIdentityTests(unittest.TestCase):
    def test_recovery_preserves_inputs_and_refuses_ambiguous_matches(self):
        with tempfile.TemporaryDirectory() as root, patch.dict(os.environ, CODEX_HOME=root):
            directory = Path(root) / 'sessions'
            directory.mkdir()
            path = directory / 'rollout-ab12.jsonl'
            def record(tool):
                return json.dumps({'payload': {'item': {'type': 'McpToolCall',
                    'server': 'queue', 'tool': tool, 'arguments': {'offset': 700}}}}) + '\n'
            event = {'type': 'tool_use', 'name': 'mcp_tool_call', 'id': 'item_1', 'input': {'offset': 700}}
            path.write_text(record('queue_read_history'))
            recovered = enrich_mcp_history([event], 'ab12')[0]
            self.assertEqual(recovered['mcp_tool'], 'queue_read_history')
            self.assertEqual(recovered['mcp_server'], 'queue')
            self.assertEqual(recovered['input'], event['input'])
            self.assertEqual(event['name'], 'mcp_tool_call')
            path.write_text(record('queue_read_history') + record('different_tool'))
            self.assertEqual(enrich_mcp_history([event], 'ab12'), [event])
