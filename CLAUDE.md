# CLAUDE.md

## Build & Test

- Build: `go build ./...`
- Test all: `go test ./...`
- Test webserver: `go test ./webserver/...`

## Rules

- Never use the `caffeinate` command.

## Web UI

The web UI lives in `webserver/`. Read `webserver/ARCHITECTURE.md` before making changes — it documents the data flow (tmux → ConversationLog → WebSocket → browser), API endpoints, design decisions, and file layout.

## Documentation

When making changes, update the relevant documentation:
- Update `CLAUDE.md` if you add new build commands, rules, or top-level project areas.
- Update `webserver/ARCHITECTURE.md` if you change the web UI: new endpoints, data flow changes, new files, or design decisions.
