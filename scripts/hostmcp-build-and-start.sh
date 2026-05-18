#!/usr/bin/env bash
#
# Build and start the docker-mcp server on the host.
# This script should be run FROM THE HOST, not inside a container.
#
# Usage:
#   ./hostmcp-build-and-start.sh              # build + start
#   ./hostmcp-build-and-start.sh --build-only # just build, don't start
#   ./hostmcp-build-and-start.sh --no-build   # skip build, just start
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MCP_DIR="$SCRIPT_DIR/../docker-mcp"
BINARY="$MCP_DIR/am-docker-mcp"

CONFIG_FILE="$MCP_DIR/config.json"
EXAMPLE_CONFIG="$MCP_DIR/docker-mcp.example.json"

LOG_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/docker-mcp/logs"

# Parse args
BUILD=true
START=true
for arg in "$@"; do
    case "$arg" in
        --build-only) START=false ;;
        --no-build)   BUILD=false ;;
        -h|--help)
            echo "Usage: $0 [--build-only | --no-build]"
            echo ""
            echo "  --build-only   Build the binary but don't start the server"
            echo "  --no-build     Skip building, just start (binary must exist)"
            echo ""
            exit 0
            ;;
        *)
            echo "Unknown argument: $arg" >&2
            exit 1
            ;;
    esac
done

# ─────────────────────────────────────────────────────────────────────────────
# Build
# ─────────────────────────────────────────────────────────────────────────────
if $BUILD; then
    echo "==> Building am-docker-mcp..."
    cd "$MCP_DIR"
    go build -o am-docker-mcp .
    echo "    Built: $BINARY"
fi

if ! $START; then
    echo "==> Build complete (--build-only). Exiting."
    exit 0
fi

# ─────────────────────────────────────────────────────────────────────────────
# Config
# ─────────────────────────────────────────────────────────────────────────────
if [[ ! -f "$CONFIG_FILE" ]]; then
    echo "==> No config found at $CONFIG_FILE"
    mkdir -p "$CONFIG_DIR"

    if [[ -f "$EXAMPLE_CONFIG" ]]; then
        echo "    Copying example config..."
        cp "$EXAMPLE_CONFIG" "$CONFIG_FILE"
        echo "    Created: $CONFIG_FILE"
        echo ""
        echo "    NOTE: Edit the config to set correct 'cwd' paths for your system!"
        echo "    Current profiles point to ~/wrk/agent_manager — adjust if needed."
        echo ""
    else
        echo "    ERROR: No example config at $EXAMPLE_CONFIG" >&2
        exit 1
    fi
fi

# Ensure log directory exists
mkdir -p "$LOG_DIR"

# ─────────────────────────────────────────────────────────────────────────────
# Token
# ─────────────────────────────────────────────────────────────────────────────
if [[ -z "${DOCKER_MCP_TOKEN:-}" ]]; then
    echo "==> DOCKER_MCP_TOKEN not set. Generating a new one..."
    export DOCKER_MCP_TOKEN="$(openssl rand -hex 32)"
    echo ""
    echo "    DOCKER_MCP_TOKEN=$DOCKER_MCP_TOKEN"
    echo ""
    echo "    Add this to your shell profile or pass to docker-compose:"
    echo "    export DOCKER_MCP_TOKEN='$DOCKER_MCP_TOKEN'"
    echo ""
fi

# ─────────────────────────────────────────────────────────────────────────────
# Verify config
# ─────────────────────────────────────────────────────────────────────────────
echo "==> Verifying config..."
if ! "$BINARY" -config "$CONFIG_FILE" -print-config > /dev/null 2>&1; then
    echo "    WARNING: Config validation had issues. Showing details:"
    "$BINARY" -config "$CONFIG_FILE" -print-config || true
    echo ""
    read -p "    Continue anyway? [y/N] " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        exit 1
    fi
else
    echo "    Config OK."
fi

# ─────────────────────────────────────────────────────────────────────────────
# Start
# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "==> Starting am-docker-mcp server..."
echo "    Config:  $CONFIG_FILE"
echo "    Logs:    $LOG_DIR"
echo "    Listen:  http://127.0.0.1:9090"
echo ""
echo "    Health check: curl http://127.0.0.1:9090/healthz"
echo "    Stop with:    Ctrl+C"
echo ""
echo "────────────────────────────────────────────────────────────────────────"

exec "$BINARY" -config "$CONFIG_FILE"
