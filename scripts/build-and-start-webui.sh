#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building claude-squad..."
go build -o bin/claude-squad .

echo "Building claude-squad-orchestrator..."
go build -o bin/claude-squad-orchestrator ./cmd/orchestrator/

echo "Build complete."
echo ""

exec bin/claude-squad serve "$@"
