"""Trusted host runner. Agent input selects fixed recipes, never shell commands."""
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys

KEY = re.compile(r"[A-Za-z0-9][A-Za-z0-9_-]{0,79}\Z")
# Timeout belongs to the container, so it survives loss of the host MCP process.
WRAPPER = '''import os,signal,subprocess,sys
p=subprocess.Popen(sys.argv[2:],start_new_session=True)
try: sys.exit(p.wait(timeout=int(sys.argv[1])))
except subprocess.TimeoutExpired:
 os.killpg(p.pid,signal.SIGKILL);p.wait();sys.exit(124)
'''
QUERY = '''import json,sys,yaml
from sqlalchemy import create_engine,text
from sqlalchemy.engine import make_url
request=json.load(sys.stdin)
config=yaml.safe_load(open(sys.argv[1]))
uri=config
for key in request['database']['config_keys']: uri=uri[key]
parsed=make_url(uri)
if parsed.get_backend_name() != 'mysql' or parsed.host != request['database']['host'] or parsed.database != request['database']['name']: raise RuntimeError('Approved database target required')
engine=create_engine(uri)
try:
 with engine.connect() as c:
  c.exec_driver_sql('SET SESSION MAX_EXECUTION_TIME=20000')
  c.exec_driver_sql('START TRANSACTION READ ONLY')
  try:
   result=c.execute(text(request['sql']))
   rows=result.fetchmany(501)
   print(json.dumps({'columns':list(result.keys()),'rows':[list(r) for r in rows[:500]],'truncated':len(rows)>500},default=str))
  finally: c.rollback()
except Exception:
 print(json.dumps({'error':'Local read-only query failed; verify SQL and container database configuration'}));sys.exit(1)
'''

def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()

class Runner:
    def __init__(self, config, cwd):
        self.c, self.cwd = config, cwd
        self.compose = [config['docker'], 'compose']
        for file in config['compose_files']:
            self.compose += ['-f', file]
        state = Path(config['state_dir']); state.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.db = sqlite3.connect(state / 'runs.sqlite', timeout=30)
        self.db.execute('CREATE TABLE IF NOT EXISTS runs (key TEXT PRIMARY KEY, recipe TEXT NOT NULL, container TEXT NOT NULL, created TEXT NOT NULL, provenance TEXT NOT NULL)')
        self.db.commit()

    def command(self, args, input=None, check=True, logs=False):
        # Bounded capture: Docker log rotation caps retained logs; commands have deadlines.
        import tempfile
        with tempfile.TemporaryFile() as out:
            p = subprocess.run(args, cwd=self.cwd, input=input, text=True, stdout=out, stderr=out if logs else subprocess.DEVNULL, timeout=60)
            out.seek(0); data = out.read(2 * 1024 * 1024 + 1)
        if len(data)>2*1024*1024: raise ValueError('Command output exceeded 2 MiB')
        if check and p.returncode: raise ValueError('Docker operation failed; inspect host Docker/configuration')
        return p.returncode, data.decode(errors='replace')

    def inspect(self, name):
        rc, raw = self.command([self.c['docker'], 'inspect', '--format', '{{json .State}}', name], check=False)
        if rc: return None
        return json.loads(raw)

    def provenance(self):
        digest = hashlib.sha256()
        root = Path(self.cwd).resolve()
        for directory in self.c.get('source_paths', []):
            source = (root / directory).resolve()
            if not source.is_relative_to(root): raise ValueError('Source path escapes working directory')
            paths = [source] if source.is_file() else sorted(source.rglob('*'))
            for path in paths:
                if path.is_file() and '__pycache__' not in path.parts and '.git' not in path.parts and (not self.c.get('source_extensions') or path.suffix in self.c['source_extensions']):
                    if not path.resolve().is_relative_to(root): raise ValueError('Source symlink escapes working directory')
                    digest.update(str(path.relative_to(root)).encode()); digest.update(path.read_bytes())
        result = {'source_sha256': digest.hexdigest(), 'checkout': self.cwd}
        if self.c.get('config_file'):
            result['config_sha256'] = hashlib.sha256(Path(self.c['config_file']).read_bytes()).hexdigest()
        if self.c.get('database'):
            result.update(database=self.c['database']['name'], database_host=self.c['database']['host'])
        return result

    def check_database(self):
        database = self.c.get('database')
        if database is None: return
        import yaml
        from urllib.parse import urlsplit
        uri = yaml.safe_load(Path(self.c['config_file']).read_text())
        for key in database['config_keys']: uri = uri[key]
        target = urlsplit(uri)
        if target.scheme.split('+')[0] != 'mysql' or target.hostname != database['host'] or target.path != '/' + database['name']:
            raise ValueError('Database target differs from approved profile')

    def run(self, request):
        action=request['action']
        if action=='query': return self.query(request.get('sql',''))
        key=request.get('run_key','')
        if not KEY.fullmatch(key): raise ValueError('run_key must be 1..80 letters/digits/underscore/hyphen')
        self.db.execute('BEGIN IMMEDIATE')
        row=self.db.execute('SELECT recipe,container,created,provenance FROM runs WHERE key=?',(key,)).fetchone()
        if action=='start':
            operation=request.get('operation','');argv=self.c['operations'].get(operation)
            if not argv: raise ValueError('Unknown approved operation')
            recipe=json.dumps({'operation':operation,'argv':argv,'compose':self.compose,'service':self.c['service'],'command_prefix':self.c.get('command_prefix',[]),'container_python':self.c.get('container_python','python3'),'timeout_seconds':self.c.get('timeout_seconds',3600)},sort_keys=True)
            if row and row[0]!=recipe: raise ValueError('run_key already belongs to a different recipe')
            if row: return self.status(key,row)
            # One active container per dataset/profile. A missing reserved container
            # also blocks another launch: uncertainty must not create duplicate writers.
            for previous in self.db.execute('SELECT container FROM runs'):
                state=self.inspect(previous[0])
                if state is None or state.get('Running') or state.get('Status')=='created':
                    raise ValueError('Another Compose run is active or needs recovery; inspect/cancel its run_key')
            name='am-compose-'+hashlib.sha256((str(Path(self.c['state_dir']).resolve())+key).encode()).hexdigest()[:24]
            self.check_database()
            created=utc();provenance=self.provenance()
            # Reserve and commit BEFORE Docker call. Ambiguous launch is never retried automatically.
            self.db.execute('INSERT INTO runs VALUES (?,?,?,?,?)',(key,recipe,name,created,json.dumps(provenance)));self.db.commit()
            command=self.compose+['run','--no-deps','-d','--name',name,'--entrypoint',self.c.get('container_python','python3'),self.c['service'],'-c',WRAPPER,str(self.c.get('timeout_seconds',3600))]+self.c.get('command_prefix',[])+argv
            self.command(command)
            row=(recipe,name,created,json.dumps(provenance))
            return self.status(key,row)
        if row is None: raise ValueError('Unknown run_key')
        if action=='status': return self.status(key,row)
        if action=='cancel':
            if self.inspect(row[1]) is None: raise ValueError('Reserved container missing; host operator must resolve uncertain launch')
            self.command([self.c['docker'],'stop','--time','10',row[1]])
            return self.status(key,row)
        if action=='logs':
            _,data=self.command([self.c['docker'],'logs','--tail','200','--timestamps',row[1]], logs=True)
            # Avoid returning URLs with embedded credentials or provider tokens.
            data=re.sub(r'https?://[^\s"\']+', '[URL redacted]',data)
            data=re.sub(r'(?i)(password|secret|token|api[_-]?key)([=:]\s*)\S+',r'\1\2[redacted]',data)
            return {'run_key':key,'tail':data,'tail_only':True}
        raise ValueError('Unknown action')

    def status(self,key,row):
        state=self.inspect(row[1]);self.db.commit()
        return {'run_key':key,'container':row[1],'created':row[2],'operation':json.loads(row[0])['operation'],'provenance':json.loads(row[3]),'state':state.get('Status') if state else 'launch_uncertain','exit_code':state.get('ExitCode') if state and state.get('Status')=='exited' else None,'finished_at':state.get('FinishedAt') if state else None}

    def query(self,sql):
        if not self.c.get('database'): raise ValueError('Database queries are not enabled for this profile')
        import sqlglot
        from sqlglot import exp
        if not sql or len(sql)>16000: raise ValueError('SQL must be 1..16000 characters')
        if any(marker in sql for marker in ('/*', '--', '#')): raise ValueError('SQL comments are not permitted')
        try: parsed=sqlglot.parse(sql,read='mysql')
        except sqlglot.errors.ParseError: raise ValueError('Invalid SELECT syntax') from None
        if len(parsed)!=1 or not isinstance(parsed[0],exp.Select): raise ValueError('One SELECT query is required')
        tree=parsed[0]
        if any(tree.find_all(exp.Into,exp.Lock,exp.Command,exp.Insert,exp.Update,exp.Delete)): raise ValueError('Only read-only SELECT is allowed')
        allowed=set(self.c['database'].get('allowed_tables',[]))
        for table in tree.find_all(exp.Table):
            if table.name not in allowed or table.catalog or table.db not in ('',self.c['database']['name']): raise ValueError('Table is not approved for this profile')
        for function in tree.find_all(exp.Func):
            if isinstance(function,exp.Anonymous): raise ValueError('Unrecognized SQL functions are not permitted')
        _,raw=self.command(self.compose+['run','--rm','--no-deps','-T','--entrypoint',self.c.get('container_python','python3'),self.c['service'],'-c',QUERY,self.c['container_config']],input=json.dumps({'sql':sql,'database':self.c['database']}))
        return json.loads(raw)


def main():
    try:
        request=json.loads(sys.stdin.buffer.read(128*1024))
        runner=Runner(request['config'],request['cwd'])
        try: result=runner.run(request)
        finally: runner.db.close()
        print(json.dumps(result))
    except Exception:
        # Never emit traceback/config/credentials from this host boundary.
        print(json.dumps({'error':'Compose request failed. Check approved operation, run status, input SQL and host configuration.'}))
        sys.exit(1)

if __name__=='__main__': main()
