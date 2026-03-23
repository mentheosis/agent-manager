#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

echo "Building claude-squad..."
go build -o bin/claude-squad .

echo "Building claude-squad-orchestrator..."
BUILD_VERSION="$(date +%Y%m%d-%H%M%S)"
go build -ldflags "-X main.Version=${BUILD_VERSION}" -o bin/claude-squad-orchestrator ./cmd/orchestrator/

echo "Build complete."
echo ""

# Ensure opencode is in PATH
export PATH="$HOME/.opencode/bin:$HOME/bin:$PATH"

exec bin/claude-squad serve "$@"
