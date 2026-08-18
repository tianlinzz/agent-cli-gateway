# syntax=docker/dockerfile:1
#
# Base image — everything except the gateway itself: the full agent runtime
# environment AND the isolation tooling. This is a PURE environment: it
# deliberately declares NO startup command (no EXPOSE / HEALTHCHECK /
# ENTRYPOINT / CMD). The product image (root Dockerfile) builds `FROM` this
# base and adds the gateway binaries + the startup contract.
#
# Ships:
#   - system: nginx, git, curl, tini, procps
#   - Python 3 + pip, with the `python` command provided by the Debian
#     python-is-python3 package (agent workloads conventionally call `python`)
#   - Go toolchain, copied from the official golang image (keep GO_VERSION in
#     sync with go.mod) so jailed agents can build/run Go code
#   - Node.js 22 LTS + npm/npx, copied from the official node image (bookworm
#     only ships Node 18, too old for the CLIs); the npm/npx symlinks are
#     recreated below because COPY dereferences them into broken files
#   - nsjail, compiled from the pinned google/nsjail tag, plus its runtime
#     shared libs
#   - agent CLIs (npm, resolved to the current latest at build time):
#     @openai/codex, @anthropic-ai/claude-code, @moonshot-ai/kimi-code
#
# Build:
#   make image-base
#   docker build --platform linux/amd64 -f docker/base/Dockerfile \
#     -t agent-gateway-base:dev .
#
# CLI versions are build args so each build controls what it ships (empty
# version = skip that CLI; resolve the npm "latest" at build time for
# always-fresh images).

ARG GO_VERSION=1.25
ARG NODE_MAJOR=22
# Pinned upstream: google/nsjail. Keep in sync with
# config/gateway.go -> IsolationConfig.NsjailVersion ("3.6").
ARG NSJAIL_VERSION=3.6
ARG NSJAIL_REF=3.6

# ---------------------------------------------------------------------------
# Stage: build nsjail
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS nsjail-builder
ARG NSJAIL_REF

# Build deps nsjail's Makefile needs (per upstream Dockerfile):
#   autoconf bison flex libprotobuf-dev libnl-route-3-dev libtool
#   pkg-config protobuf-compiler, plus gcc/g++/make/git and curl to fetch.
RUN apt-get update && apt-get install -y --no-install-recommends \
        autoconf \
        bison \
        flex \
        gcc \
        g++ \
        git \
        libprotobuf-dev \
        libnl-route-3-dev \
        libtool \
        make \
        pkg-config \
        protobuf-compiler \
        curl \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /nsjail
RUN git clone --depth 1 --branch "${NSJAIL_REF}" \
        https://github.com/google/nsjail.git . \
    && make clean \
    && make \
    && test -x ./nsjail

FROM golang:${GO_VERSION}-bookworm AS go-toolchain
FROM node:${NODE_MAJOR}-bookworm-slim AS node-runtime

# ---------------------------------------------------------------------------
# Final: the environment base (no startup command)
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

# System runtimes, nsjail runtime libs, and PID-1/tooling the product image
# and the jailed workers rely on. python-is-python3 provides the `python`
# command via the alternatives system.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        libc6 \
        libnl-route-3-200 \
        libprotobuf32 \
        libstdc++6 \
        nginx \
        procps \
        python-is-python3 \
        python3 \
        python3-pip \
        tini \
    && rm -rf /var/lib/apt/lists/*

# nsjail + its runtime shared-lib deps. ldd is used so the image never carries
# stale/copied libs that drift from the compiled binary.
COPY --from=nsjail-builder /nsjail/nsjail /usr/local/bin/nsjail
RUN set -eux; \
    mkdir -p /opt/nsjail-libs; \
    for lib in $(ldd /usr/local/bin/nsjail | awk '/=> \//{print $3}' | sort -u); do \
        cp "$lib" /opt/nsjail-libs/; \
    done; \
    chmod 0755 /usr/local/bin/nsjail

# Go toolchain (GOROOT copy from the official image — hermetic, no apt repo).
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV GOROOT=/usr/local/go
ENV PATH=/usr/local/go/bin:${PATH}

# Node.js runtime + npm/npx aliases into node_modules.
COPY --from=node-runtime /usr/local/bin/node /usr/local/bin/node
COPY --from=node-runtime /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -sf /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
 && ln -sf /usr/local/lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

# --- Agent CLIs (per-provider, network required at build time) ---------------
# An empty version arg skips that provider.
ARG CODEX_VERSION=""
ARG CLAUDE_VERSION=""
ARG KIMI_VERSION=""
RUN if [ -n "${CODEX_VERSION}" ]; then npm install -g "@openai/codex@${CODEX_VERSION}"; fi \
 && if [ -n "${CLAUDE_VERSION}" ]; then npm install -g "@anthropic-ai/claude-code@${CLAUDE_VERSION}"; fi \
 && if [ -n "${KIMI_VERSION}" ]; then npm install -g "@moonshot-ai/kimi-code@${KIMI_VERSION}"; fi

# Deliberately NO EXPOSE / HEALTHCHECK / ENTRYPOINT / CMD: environment only.
# The startup contract lives in the product image (root Dockerfile).
