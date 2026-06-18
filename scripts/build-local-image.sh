#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ ! -f Dockerfile.local ]]; then
    cp Dockerfile.local.example Dockerfile.local
fi

docker build -t agent-manager:base .
docker build \
    -f Dockerfile.local \
    --build-arg BASE_IMAGE=agent-manager:base \
    -t agent-manager:local \
    .

cat <<'EOF'
Built agent-manager:local.

To run it with compose, set this in docker-compose.local.yml:

services:
  agent-manager:
    build:
      context: .
      dockerfile: Dockerfile.local
      args:
        BASE_IMAGE: agent-manager:base
    image: agent-manager:local
EOF
