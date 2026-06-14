# syntax=docker/dockerfile:1

# =============================================================================
# Stage 1: Go build (orchestrator binary)
# =============================================================================
FROM golang:1.22-alpine AS go-deps
WORKDIR /build
# Copy only go.mod first to cache dependency download
COPY orchestrator/go.mod orchestrator/go.sum* ./
RUN go mod download

FROM go-deps AS go-build
# Copy source and build
COPY orchestrator/ ./
RUN CGO_ENABLED=0 go build -o am-orchestrator .

FROM go-deps AS go-test
COPY orchestrator/ ./
RUN go test ./...

# =============================================================================
# Stage 2: Python application
# =============================================================================
FROM python:3.12-slim AS python-app

# --- System packages (changes rarely) -------------------------------------
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        curl \
        gnupg \
        git \
        ripgrep \
        ca-certificates \
        build-essential \
        pkg-config \
        libssl-dev \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# --- Create non-root user with /app as home ------------------------------
ARG UID=1000
ARG GID=1000
RUN mkdir -p /app/.claude/backups /app/.codex /var/lib/agent-manager \
    && groupadd -g ${GID} agent \
    && useradd -u ${UID} -g agent -s /bin/bash -d /app agent \
    && chown -R agent:agent /app /var/lib/agent-manager

# --- Rust toolchain (installed for agent user) ----------------------------
USER agent
WORKDIR /app
ENV HOME=/app \
    RUSTUP_HOME=/app/.rustup \
    CARGO_HOME=/app/.cargo \
    PATH="/app/.cargo/bin:${PATH}"
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y --default-toolchain stable

# --- Provider CLIs (changes rarely) ----------------------------------------
USER root
RUN npm install -g @anthropic-ai/claude-code @openai/codex

# --- Python deps (rebuilds only when pyproject.toml changes) --------------
# Install only the dependency list — not the project itself — so that
# editing source or static files doesn't invalidate this layer.
COPY --chown=agent:agent pyproject.toml ./
RUN python -c "import tomllib; d = tomllib.load(open('pyproject.toml','rb')); print('\n'.join(d['project']['dependencies']))" \
        > /tmp/deps.txt \
    && pip install --no-cache-dir -r /tmp/deps.txt \
    && rm /tmp/deps.txt

# --- Project source (rebuilds on src/ changes) ----------------------------
COPY --chown=agent:agent README.md ./
COPY --chown=agent:agent src/ ./src/
RUN pip install --no-cache-dir --no-deps .

FROM python-app AS python-test
COPY --chown=agent:agent tests/ ./tests/
COPY --chown=agent:agent static/ ./static/
RUN pip install --no-cache-dir -e ".[dev]"
RUN pytest

# --- Static assets last (rebuilds only on static/ changes) ----------------
COPY --chown=agent:agent static/ ./static/

# --- Orchestrator binary (from Go build stage) -----------------------------
COPY --from=go-build /build/am-orchestrator /usr/local/bin/

# --- Switch to non-root user ----------------------------------------------
USER agent
WORKDIR /app

EXPOSE 8787
ENV AGENT_MANAGER_HOST=0.0.0.0 \
    PYTHONUNBUFFERED=1

CMD ["agent-manager"]
