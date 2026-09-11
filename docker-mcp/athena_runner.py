"""Host-only Athena adapter. Receives server-generated JSON on stdin, never shell SQL."""
import json
import signal
import sys
import time

import boto3
from botocore.config import Config
import sqlglot
from sqlglot import exp
from sqlglot.optimizer.scope import traverse_scope


def validate_sql(sql, config):
    if not isinstance(sql, str) or not 0 < len(sql.encode()) <= 65536:
        raise ValueError('SQL must be 1..65536 bytes')
    statements = sqlglot.parse(sql, read='trino')
    if len(statements) != 1 or not isinstance(statements[0], (exp.Select, exp.Union, exp.Intersect, exp.Except)):
        raise ValueError('Exactly one SELECT query is permitted')
    tree = statements[0]
    # Fail closed on commands, external functions, and table-valued functions.
    forbidden = {'Command', 'Insert', 'Update', 'Delete', 'Create', 'Drop', 'Alter',
                 'Merge', 'Into', 'Copy', 'Execute', 'Use', 'Grant', 'Revoke', 'Anonymous'}
    for node in tree.walk():
        if type(node).__name__ in forbidden:
            raise ValueError('Unsupported SQL construct: ' + type(node).__name__)
    allowed = {t.lower() for t in config['allowed_tables']}
    for scope in traverse_scope(tree):
        for _, source in scope.sources.items():
            if isinstance(source, exp.Table):
                if not isinstance(source.this, exp.Identifier):
                    raise ValueError('Table functions are not permitted')
                catalog = source.catalog or config['catalog']
                database = source.db or config['database']
                name = f'{catalog}.{database}.{source.name}'.lower()
                if name not in allowed:
                    raise ValueError('Table not allowed: ' + name)
    return sql


def run(request):
    config = request['config']
    sql = validate_sql(request['sql'], config)
    limit = request['max_rows']
    if not isinstance(limit, int) or not 1 <= limit <= 1000:
        raise ValueError('max_rows must be 1..1000')
    timeout = request['timeout_seconds']
    if not 1 <= timeout <= 590:
        raise ValueError('invalid timeout')
    client = boto3.Session(profile_name=config['aws_profile'], region_name=config['region']).client(
        'athena', config=Config(connect_timeout=1, read_timeout=2, retries={'max_attempts': 0}))
    query_id = None
    complete = False
    def interrupted(*_):
        raise TimeoutError('query cancelled or timed out')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGALRM, interrupted)
    signal.alarm(timeout)
    try:
        kwargs = dict(QueryString=sql, QueryExecutionContext={'Catalog': config['catalog'], 'Database': config['database']},
                      WorkGroup=config['workgroup'])
        if config.get('output_location'):
            kwargs['ResultConfiguration'] = {'OutputLocation': config['output_location']}
        query_id = client.start_query_execution(**kwargs)['QueryExecutionId']
        print(json.dumps({'query_id': query_id, 'state': 'SUBMITTED'}), flush=True)
        while True:
            execution = client.get_query_execution(QueryExecutionId=query_id)['QueryExecution']
            state = execution['Status']['State']
            if state == 'SUCCEEDED':
                complete = True
                break
            if state in ('FAILED', 'CANCELLED'):
                complete = True
                raise RuntimeError(execution['Status'].get('StateChangeReason', state))
            time.sleep(1)
        rows, columns, token = [], [], None
        size, first, truncated = 0, True, False
        while True:
            kwargs = dict(QueryExecutionId=query_id, MaxResults=min(1000, limit + 1))
            if token:
                kwargs['NextToken'] = token
            page = client.get_query_results(**kwargs)
            result = page['ResultSet']
            columns = result['ResultSetMetadata']['ColumnInfo']
            data = result.get('Rows', [])
            if first:
                data = data[1:]  # Athena's first row is the column header.
                first = False
            for row in data:
                values = [v.get('VarCharValue') for v in row['Data']]
                size += len(json.dumps(values).encode())
                if len(rows) >= limit or size > 512000:
                    truncated = True
                    break
                rows.append(values)
            token = page.get('NextToken')
            if truncated or not token:
                break
        print(json.dumps({'query_id': query_id, 'state': state, 'columns': columns,
                          'rows': rows, 'truncated': truncated, 'statistics': execution.get('Statistics', {})}), flush=True)
    finally:
        signal.alarm(0)
        if query_id and not complete:
            # Fit cancellation within the host job manager's five-second kill grace.
            client.stop_query_execution(QueryExecutionId=query_id)


if __name__ == '__main__':
    try:
        raw = sys.stdin.buffer.read(131073)
        if len(raw) > 131072:
            raise ValueError('request too large')
        run(json.loads(raw))
    except Exception as exc:
        print(json.dumps({'error': str(exc)}), flush=True)
        sys.exit(1)
