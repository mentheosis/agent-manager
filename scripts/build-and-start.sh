#!/usr/bin/env bash
set -euo pipefail

COMPOSE_FILES=(-f docker-compose.yml)
if [[ -f docker-compose.local.yml ]]; then
    COMPOSE_FILES+=(-f docker-compose.local.yml)
fi

docker compose "${COMPOSE_FILES[@]}" up --build -d

echo "agent-manager is running in the background at http://127.0.0.1:8787"
echo "logs: docker compose ${COMPOSE_FILES[*]} logs -f agent-manager"
echo "stop: docker compose ${COMPOSE_FILES[*]} down"
