#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

COMPOSE_FILES=(-f docker-compose.yml -f docker-compose.local.yml)

if [[ -f Dockerfile.local ]] \
    && grep -q 'Dockerfile.local' docker-compose.local.yml; then
    docker build -t agent-manager:base .
fi

docker compose "${COMPOSE_FILES[@]}" up --build -d

echo "agent-manager is running in the background at http://127.0.0.1:8787"
echo "logs: docker compose ${COMPOSE_FILES[*]} logs -f agent-manager"
echo "stop: docker compose ${COMPOSE_FILES[*]} down"
