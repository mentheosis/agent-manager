#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [[ ! -f Dockerfile.local ]]; then
    cp Dockerfile.local.example Dockerfile.local
fi

# Test stages are not ancestors of the runtime target, so BuildKit skips
# them on a plain build — run them explicitly to keep tests gating the image.
docker build --target go-test -t agent-manager:go-test .
docker build --target python-test -t agent-manager:test .

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
