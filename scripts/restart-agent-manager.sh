#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ ! -f Dockerfile.local ]]; then
    cp Dockerfile.local.example Dockerfile.local
fi

if [[ ! -f docker-compose.local.yml ]]; then
    cp docker-compose.local.yml.example docker-compose.local.yml
fi

docker compose -f docker-compose.yml -f docker-compose.local.yml down
./scripts/build-and-start.sh
