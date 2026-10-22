# Session container: one Claude Code session in tmux, isolated by the
# spawner's run template (read-only root, no capabilities, uid 1500).
FROM golang:1.26-bookworm AS coherence
WORKDIR /src
COPY coherence/ .
RUN CGO_ENABLED=0 go build -o /out/coherence-doc ./cmd/coherence-doc

FROM node:22-bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends tini tmux git ca-certificates jq ripgrep curl procps \
 && rm -rf /var/lib/apt/lists/*
# Pinned: channels are a research preview; upgrades go through an approved PR.
ARG CLAUDE_CODE_VERSION=2.1.308
RUN npm install -g --no-audit --no-fund @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} && npm cache clean --force
COPY aejar-mcp/package.json aejar-mcp/package-lock.json /opt/aejar/mcp/
RUN cd /opt/aejar/mcp && npm ci --omit=dev --no-audit --no-fund && npm cache clean --force
COPY aejar-mcp/server.mjs /opt/aejar/mcp/
COPY images/session/entry.sh images/session/launch.mjs images/session/hook-post.mjs /opt/aejar/bin/
COPY --from=coherence /out/coherence-doc /usr/local/bin/coherence-doc
RUN useradd --uid 1500 --create-home --home-dir /home/agent agent \
 && mkdir -p /work /run/aejar /etc/aejar && chown agent:agent /work /run/aejar \
 && chmod 0755 /opt/aejar/bin/entry.sh
ENV TERM=xterm-256color
USER agent
WORKDIR /work
ENTRYPOINT ["/usr/bin/tini", "--", "/opt/aejar/bin/entry.sh"]
