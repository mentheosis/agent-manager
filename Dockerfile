# syntax=docker/dockerfile:1
FROM python:3.12-slim

# --- System packages (changes rarely) -------------------------------------
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        curl \
        gnupg \
        git \
        ca-certificates \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# --- Claude CLI (changes rarely) ------------------------------------------
RUN npm install -g @anthropic-ai/claude-code

WORKDIR /app

# --- Python deps (rebuilds only when pyproject.toml changes) --------------
# Install only the dependency list — not the project itself — so that
# editing source or static files doesn't invalidate this layer.
COPY pyproject.toml ./
RUN python -c "import tomllib; d = tomllib.load(open('pyproject.toml','rb')); print('\n'.join(d['project']['dependencies']))" \
        > /tmp/deps.txt \
    && pip install --no-cache-dir -r /tmp/deps.txt \
    && rm /tmp/deps.txt

# --- Project source (rebuilds on src/ changes) ----------------------------
COPY README.md ./
COPY src/ ./src/
RUN pip install --no-cache-dir --no-deps .

# --- Static assets last (rebuilds only on static/ changes) ----------------
COPY static/ ./static/

EXPOSE 8787
ENV AGENT_MANAGER_HOST=0.0.0.0 \
    PYTHONUNBUFFERED=1

CMD ["agent-manager"]
