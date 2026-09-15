import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec=importlib.util.spec_from_file_location('compose_runner',Path(__file__).with_name('compose_runner.py'))
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)

class ComposeTests(unittest.TestCase):
 def setUp(self):
  self.tmp=tempfile.TemporaryDirectory();self.root=Path(self.tmp.name)
  config=self.root/'local.yaml';config.write_text('application:\n  databases:\n    sampledb:\n      sqlalchemy:\n        uri: mysql://user:secret@sample-db/sampledb\n')
  self.config={'state_dir':str(self.root/'state'),'docker':'/docker','compose_files':['/compose.yml'],'service':'application','config_file':str(config),'container_config':'/local.yaml','operations':{'extract':['-c','local.yaml','chains','Example','v1','extract']},'database':{'config_keys':['application','databases','sampledb','sqlalchemy','uri'],'host':'sample-db','name':'sampledb','allowed_tables':['holding']},'command_prefix':['/usr/local/bin/sample-entrypoint'],'timeout_seconds':60}
  self.runner=m.Runner(self.config,str(self.root))
 def tearDown(self):self.runner.db.close();self.tmp.cleanup()
 def test_start_is_durable_and_idempotent(self):
  with patch.object(self.runner,'command',return_value=(0,'container')) as command,patch.object(self.runner,'inspect',return_value={'Status':'running','Running':True}):
   first=self.runner.run({'action':'start','run_key':'pilot-extract','operation':'extract'})
   second=self.runner.run({'action':'start','run_key':'pilot-extract','operation':'extract'})
   self.assertEqual(first['container'],second['container']);self.assertEqual(command.call_count,1)
  self.runner.db.close();self.runner=m.Runner(self.config,str(self.root))
  with patch.object(self.runner,'inspect',return_value={'Status':'exited','ExitCode':0}):
   self.assertEqual(self.runner.run({'action':'status','run_key':'pilot-extract'})['exit_code'],0)
 def test_ambiguous_launch_does_not_relaunch(self):
  with patch.object(self.runner,'command',side_effect=ValueError('lost')):
   with self.assertRaises(ValueError):self.runner.run({'action':'start','run_key':'one','operation':'extract'})
  with patch.object(self.runner,'inspect',return_value=None),patch.object(self.runner,'command') as command:
   self.assertEqual(self.runner.run({'action':'start','run_key':'one','operation':'extract'})['state'],'launch_uncertain')
   with self.assertRaises(ValueError):self.runner.run({'action':'start','run_key':'two','operation':'extract'})
   command.assert_not_called()
 def test_rejects_arbitrary_recipe_and_run_key(self):
  for key,op in [('../bad','extract'),('good','sh')]:
   with self.assertRaises(ValueError):self.runner.run({'action':'start','run_key':key,'operation':op})
   self.runner.db.rollback()
 def test_query_restrictions(self):
  for sql in ['DELETE FROM holding','SELECT * FROM other','SELECT * FROM prod.holding','SELECT * FROM holding FOR UPDATE','SELECT SLEEP(10)','SELECT 1; SELECT 2', 'SELECT 1 /* executable comment */',"SELECT * INTO OUTFILE '/tmp/a' FROM holding"]:
   with self.subTest(sql=sql),self.assertRaises(ValueError):self.runner.query(sql)
  with patch.object(self.runner,'command',return_value=(0,json.dumps({'rows':[[1]]}))) as command:
   self.assertEqual(self.runner.query('SELECT COUNT(*) FROM holding')['rows'],[[1]])
   argv=command.call_args.args[0];self.assertIn('--entrypoint',argv);self.assertIn('-T',argv)
 def test_cancel_targets_container(self):
  with patch.object(self.runner,'command',return_value=(0,'')),patch.object(self.runner,'inspect',return_value={'Status':'running','Running':True}):
   result=self.runner.run({'action':'start','run_key':'one','operation':'extract'})
  with patch.object(self.runner,'command',return_value=(0,'')) as command,patch.object(self.runner,'inspect',return_value={'Status':'exited','ExitCode':137}):
   self.runner.run({'action':'cancel','run_key':'one'})
   self.assertEqual(command.call_args.args[0],['/docker','stop','--time','10',result['container']])

if __name__=='__main__':unittest.main()

class GenericProfileTests(unittest.TestCase):
 def test_command_only_profile_needs_no_application_or_database_config(self):
  with tempfile.TemporaryDirectory() as root:
   config={'state_dir':root,'docker':'/docker','compose_files':['/compose.yml'],'service':'worker','operations':{'check':['/bin/echo','ok']},'timeout_seconds':60}
   runner=m.Runner(config,root)
   try:
    with patch.object(runner,'command',return_value=(0,'')) as command,patch.object(runner,'inspect',return_value={'Status':'running','Running':True}):
     result=runner.run({'action':'start','run_key':'check-one','operation':'check'})
     self.assertEqual(command.call_args.args[0][-2:],['/bin/echo','ok'])
     self.assertTrue(result['container'].startswith('am-compose-'))
     self.assertNotIn('database',result['provenance'])
    with self.assertRaises(ValueError):runner.query('SELECT 1')
   finally:runner.db.close()
 def test_database_keys_and_target_are_profile_defined(self):
  with tempfile.TemporaryDirectory() as root:
   config_file=Path(root)/'settings.yml';config_file.write_text('custom:\n  url: mysql://u:p@other-db/other\n')
   config={'state_dir':str(Path(root)/'state'),'docker':'/docker','compose_files':['/compose.yml'],'service':'worker','operations':{'check':['echo']},'config_file':str(config_file),'database':{'config_keys':['custom','url'],'host':'other-db','name':'other','allowed_tables':['events']}}
   runner=m.Runner(config,root)
   try:
    runner.check_database()
    config['database']['host']='wrong-db'
    with self.assertRaises(ValueError):runner.check_database()
   finally:runner.db.close()
