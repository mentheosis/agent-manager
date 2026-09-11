import unittest
from unittest.mock import Mock, patch
import athena_runner as runner

CFG = dict(aws_profile='audit', region='eu-west-1', catalog='awsdatacatalog', database='prod',
           workgroup='audit', allowed_tables=['awsdatacatalog.prod.transactions'])

class ValidationTests(unittest.TestCase):
    def test_reads(self):
        for sql in ['SELECT 1', 'SELECT sum(rewards) FROM transactions',
                    'WITH t AS (SELECT * FROM transactions) SELECT * FROM t',
                    'SELECT * FROM prod.transactions UNION ALL SELECT * FROM transactions',
                    "SELECT '$(touch /tmp/bad); DROP TABLE x' AS literal"]:
            with self.subTest(sql=sql):
                self.assertEqual(runner.validate_sql(sql, CFG), sql)

    def test_rejections(self):
        for sql in ['SELECT 1; SELECT 2', 'DROP TABLE transactions',
                    'CREATE TABLE x AS SELECT * FROM transactions',
                    "UNLOAD (SELECT * FROM transactions) TO 's3://x/' WITH (format='PARQUET')",
                    'SELECT * FROM other.transactions', 'SELECT * FROM other.prod.transactions',
                    'WITH transactions AS (SELECT * FROM secret) SELECT * FROM transactions',
                    'SELECT evil_function(x) FROM transactions',
                    "SELECT * FROM TABLE(system.query(query => 'DELETE FROM x'))",
                    'SELECT * INTO x FROM transactions']:
            with self.subTest(sql=sql), self.assertRaises(Exception):
                runner.validate_sql(sql, CFG)

    @patch.object(runner.boto3, 'Session')
    def test_results_preserve_strings_and_truncate(self, session):
        client = session.return_value.client.return_value
        client.start_query_execution.return_value = {'QueryExecutionId': 'q'}
        client.get_query_execution.return_value = {'QueryExecution': {'Status': {'State': 'SUCCEEDED'}}}
        client.get_query_results.return_value = {'ResultSet': {'ResultSetMetadata': {'ColumnInfo': []},
            'Rows': [{'Data': [{'VarCharValue': v}]} for v in ['header', '12345678901234567890', '2']]}}
        with patch('builtins.print') as output:
            runner.run(dict(config=CFG, sql='SELECT 1', max_rows=1, timeout_seconds=5))
        import json
        result = json.loads(output.call_args.args[0])
        self.assertEqual(result['rows'], [['12345678901234567890']])
        self.assertTrue(result['truncated'])
        client.stop_query_execution.assert_not_called()

    @patch.object(runner.boto3, 'Session')
    def test_cancel_on_poll_error(self, session):
        client = session.return_value.client.return_value
        client.start_query_execution.return_value = {'QueryExecutionId': 'q'}
        client.get_query_execution.side_effect = TimeoutError('expired')
        with self.assertRaises(TimeoutError), patch('builtins.print'):
            runner.run(dict(config=CFG, sql='SELECT 1', max_rows=1, timeout_seconds=5))
        client.stop_query_execution.assert_called_once_with(QueryExecutionId='q')

if __name__ == '__main__':
    unittest.main()
