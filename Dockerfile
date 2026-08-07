# syntax=docker/dockerfile:1
#
# agent-cli-gateway — OpenAI-compatible Agent Gateway (nsjail-isolated workers)
#
# Multi-stage build:
#   Stage 1 (nsjail-builder): compiles google/nsjail from a pinned upstream
#     tag/commit and packages the binary plus its runtime shared-library deps.
#   Stage 2 (gobuild): builds the `gateway` (API + worker supervisor) and
#     `gateway-worker` (per-session worker child) binaries from this repo.
#   Stage 3 (runtime): the deployable image — nsjail + Go binaries + the agent
#     CLIs the deployment needs. The gateway process is PID 1; it owns the
#     worker Supervisor, which forks one nsjail-wrapped `gateway-worker` per
#     session.
#
# Security model (决策对齐 #5b / the rewrite design spec):
#   - nsjail runs UNPRIVILEGED: user namespace, NO CAP_SYS_ADMIN, NOT
#     --privileged. The container itself is non-root when the platform allows
#     running without the sandbox privileges nsjail needs for user namespaces;
#     the default k8s securityContext below encodes this.
#   - Phase 1 keeps the network namespace shared (agents must reach their
#     providers); egress is controlled by the container/infrastructure.
#
# Build:
#   docker build -t agent-gateway .
#
# Run (docker):
#   docker run --rm -it \
#     --cap-drop=ALL \
#     -v "$PWD/workspaces:/srv/workspaces" \
#     -p 4096:4096 agent-gateway
#
# Run (kubernetes, minimal security context):
#   securityContext:
#     runAsNonRoot: true
#     runAsUser: 65532
#     runAsGroup: 65532
#     allowPrivilegeEscalation: false
#     capabilities: { drop: [ALL] }        # nsjail uses unprivileged user ns
#     seccompProfile: { type: RuntimeDefault }
#
# CI validation hooks (Linux CI only — nsjail cannot run on this darwin dev
# host):
#   - `nsjail --version`                    # binary runs
#   - `ldd /usr/local/bin/nsjail`           # no missing shared libs
#   - minimal smoke jail, e.g.
#       echo 'mode: ONCE; clone_newns: true;' | nsjail -Mo --config /dev/stdin -- /bin/true
#   See docker/nsjail-smoke.sh (runs the above in one script).

# ---------------------------------------------------------------------------
# Stage 1: build nsjail
# ---------------------------------------------------------------------------
# Pinned upstream: google/nsjail. Keep in sync with
# config/gateway.go -> IsolationConfig.NsjailVersion ("0.12.0").
ARG NSJAIL_VERSION=0.12.0
ARG NSJAIL_REF=0.12.0

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

# ---------------------------------------------------------------------------
# Stage 2: build the Go binaries
# ---------------------------------------------------------------------------
FROM golang:1.25-bookworm AS gobuild
ARG GOPROXY_DEFAULT=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY_DEFAULT} \
    CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# gateway (API + supervisor) and gateway-worker (per-session worker child).
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway \
    && go build -trimpath -ldflags="-s -w" -o /out/gateway-worker ./cmd/gateway-worker

# ---------------------------------------------------------------------------
# Stage 3: runtime image
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

# Runtime libs nsjail needs (mirrors upstream Dockerfile's runtime stage) plus
# tools the agent CLIs commonly shell out to (git, python3, curl).
RUN apt-get update && apt-get install -y --no-install-recommends \
        libc6 \
        libstdc++6 \
        libprotobuf32 \
        libnl-route-3-200 \
        ca-certificates \
        curl \
        git \
        python3 \
    && rm -rf /var/lib/apt/lists/*

# nsjail + its runtime shared-lib deps. ldd is used so the image never carries
# stale/copied libs that drift from the compiled binary. The explicit apt
# runtime libs above cover the same set; the ldd copy is the source of truth.
COPY --from=nsjail-builder /nsjail/nsjail /usr/local/bin/nsjail
RUN set -eux; \
    mkdir -p /opt/nsjail-libs; \
    for lib in $(ldd /usr/local/bin/nsjail | awk '/=> \//{print $3}' | sort -u); do \
        cp "$lib" /opt/nsjail-libs/; \
    done; \
    chmod 0755 /usr/local/bin/nsjail

# Go binaries: gateway (API + supervisor) and gateway-worker (worker child).
COPY --from=gobuild /out/gateway /usr/local/bin/gateway
COPY --from=gobuild /out/gateway-worker /usr/local/bin/gateway-worker

# --- Agent CLIs (per deployment model) --------------------------------------
# The deployment model ships the CLIs the enabled agents need. Examples
# (network-dependent; enable what the deployment uses and pin versions):
#
#   # Codex (npm):
#   RUN npm install -g @openai/codex@<pin>
#
#   # Claude Code (npm):
#   RUN npm install -g @anthropic-ai/claude-code@<pin>
#
#   # Kimi CLI (npm):
#   RUN npm install -g <kimi-cli-package>@<pin>
#
# These are intentionally left commented: the exact packages/pins vary by
# deployment, they need network at build time, and this image's base runtime
# is validated by the Go + nsjail smoke path regardless.

# --- Runtime layout ---------------------------------------------------------
# The gateway and its worker children share:
#   /srv/workspaces  <- controlled workspace root (all workspace_ids resolve
#                       under it; bind-mount a persistent volume here)
#   /gateway-run     <- per-session sockets/agent-homes/profiles (tmpfs OK)
# Both are created at boot by the entrypoint (idempotent, works on a fresh
# mounted volume).
ENV GW_WORKER_EXEC=/usr/local/bin/gateway-worker \
    GATEWAY_CONFIG=/etc/gateway/gateway.toml

EXPOSE 4096

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:4096/health/live || exit 1

# Minimal signal-aware entrypoint: it exec's the gateway binary so the gateway
# (which owns the worker Supervisor) is PID 1 and receives SIGTERM directly.
# SIGTERM propagation API -> worker(nsjail) -> CLI is handled inside the
# supervisor (CloseSession RPC, SIGTERM to the process group, SIGKILL
# escalation, group reap).
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["gateway"]
