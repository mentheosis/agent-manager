"""Explicit schema setup using the same protected profiles as the web supervisor."""
import argparse
import json
import os
import subprocess

from ..orchestrator import OrchestratorManager
from .controllers import queue_config


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('command', choices=['init-schema'])
    parser.add_argument('--profile', required=True)
    parser.add_argument('--binary', help='Override the installed am-orchestrator path')
    args = parser.parse_args()
    try:
        config = queue_config(args.profile)
        binary = args.binary or OrchestratorManager().find_binary()
        if not binary:
            parser.error('am-orchestrator binary is unavailable; rebuild it first')
        result = subprocess.run([binary, '--mode', 'task-queue', '--init-schema'],
            env={**os.environ, 'AM_QUEUE_CONFIG': json.dumps(config)}, check=False)
    except (ValueError, OSError) as exc:
        parser.error(str(exc))
    raise SystemExit(result.returncode)


if __name__ == '__main__':
    main()
