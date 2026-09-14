"""Initialize schema, render assignments and enqueue batches using protected profiles."""
import argparse
import json
import os
import subprocess

from ..orchestrator import OrchestratorManager
from .controllers import queue_config


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['init-schema', 'render', 'enqueue'])
    parser.add_argument('--profile', required=True)
    parser.add_argument('--binary', help='Override the installed am-orchestrator path')
    parser.add_argument('--file', help='Batch JSON file; defaults to stdin for render/enqueue')
    args = parser.parse_args()
    try:
        config = queue_config(args.profile)
        binary = args.binary or OrchestratorManager().find_binary()
        if not binary:
            parser.error('am-orchestrator binary is unavailable; rebuild it first')
        command = [binary, '--mode', 'task-queue']
        if args.command == 'init-schema':
            command.append('--init-schema')
            result = subprocess.run(command, env={**os.environ, 'AM_QUEUE_CONFIG': json.dumps(config)}, check=False)
        else:
            command.extend(['--queue-action', args.command])
            if args.file:
                with open(args.file, 'rb') as batch:
                    result = subprocess.run(command, stdin=batch, env={**os.environ, 'AM_QUEUE_CONFIG': json.dumps(config)}, check=False)
            else:
                result = subprocess.run(command, env={**os.environ, 'AM_QUEUE_CONFIG': json.dumps(config)}, check=False)
    except (ValueError, OSError) as exc:
        parser.error(str(exc))
    raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
