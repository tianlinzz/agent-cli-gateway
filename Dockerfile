# syntax=docker/dockerfile:1
#
# agent-cli-gateway — OpenAI-compatible Agent Gateway (nsjail-isolated workers)
#
# PRODUCT image: builds on the base image (docker/base/Dockerfile — nginx, Go,
# Node 22, Python3, the agent CLIs, and nsjail) and adds everything this
# product owns:
#   Stage 1 (gobuild): builds the `gateway` (API + worker supervisor) and
#     `gateway-worker` (per-session worker child) binaries from this repo.
#   Stage 2 (runtime): FROM the base image — the Go binaries + the startup
#     contract. Tini is PID 1 and launches the gateway, whose Supervisor
#     forks one nsjail-wrapped `gateway-worker` per session.
#
# Security model (决策对齐 #5b / the rewrite design spec):
#   - nsjail runs UNPRIVILEGED: user namespace, NO CAP_SYS_ADMIN, NOT
#     --privileged. The container itself is non-root when the platform allows
#     running without the sandbox privileges nsjail needs for user namespaces;
#     the default k8s securityContext below encodes this.
#   - Phase 1 keeps the network namespace shared (agents must reach their
#     providers); egress is controlled by the container/infrastructure.
#
# Build (first build the base image, then this product image; on a non-amd64
# host pass --platform linux/amd64 to BOTH builds — see make image-*):
#   make image-base
#   make image-product
#   # or directly:
#   docker build --platform linux/amd64 \
#     --build-arg BASE_IMAGE=agent-gateway-base:dev \
#     -t agent-gateway:dev .
#
# Run (docker):
#   docker run --rm -it \
#     --cap-drop=ALL \
#     -v "$PWD/workspaces:/srv/workspaces" \
#     -p 4096:4096 agent-gateway:dev
#
# Run (docker, restricted hosts — compat mode): on hosts whose LSM denies
# mounts in NON-root-created user namespaces (Ubuntu >= 24.04 userns
# restriction) and whose runtime masks /proc, run as root and switch the
# jail's /proc to a bind mount (gateway.toml: [isolation.mounts]
# proc_mount = "bind"). The jail still de-privileges agents to uid 65532;
# the gateway chowns the dirs the jail must access. See README "Restricted
# hosts (root-run compat mode)":
#   docker run --rm -it \
#     --user 0:0 \
#     --cap-drop=ALL \
#     -v "$PWD/workspaces:/srv/workspaces" \
#     -p 4096:4096 agent-gateway:dev
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
# host; the nsjail binary itself comes from the base image):
#   - `nsjail --version`                    # binary runs
#   - `ldd /usr/local/bin/nsjail`           # no missing shared libs
#   - minimal smoke jail, e.g.
#       echo 'mode: ONCE; clone_newns: true;' | nsjail -Mo --config /dev/stdin -- /bin/true
#   See docker/nsjail-smoke.sh (runs the above in one script).

# ---------------------------------------------------------------------------
# Stage 1: build the Go binaries
# ---------------------------------------------------------------------------
# NOTE: ARGs usable in FROM lines must be declared BEFORE the first FROM.
# Base image the runtime stage builds on (built by `make image-base`).
ARG BASE_IMAGE=agent-gateway-base:dev

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
# Stage 2: product runtime (base image + Go binaries + startup contract)
# ---------------------------------------------------------------------------
# The base image (docker/base/Dockerfile) provides nginx, Go, Node 22,
# Python3, nsjail + its libs, and the agent CLIs (codex, claude, kimi — built
# with the npm latest of each build). Every agent still defaults to DISABLED
# until a config enables it (`[agents.<id>] enabled = true` + `command`), so
# /v1/models advertises nothing out of the box.
FROM ${BASE_IMAGE}

# The gateway process runs from /: the runtime-dir default ("gateway-run"
# under cwd) must land on the ephemeral container layer, never on the
# workspace volume. All data locations are absolute, from the config.
WORKDIR /

# Go binaries: gateway (API + supervisor) and gateway-worker (worker child).
COPY --from=gobuild /out/gateway /usr/local/bin/gateway
COPY --from=gobuild /out/gateway-worker /usr/local/bin/gateway-worker

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

# Tini is PID 1: it forwards signals and reaps orphaned descendants. The shell
# entrypoint prepares configuration and execs Gateway; Gateway's Supervisor
# owns session-aware propagation API -> worker(nsjail) -> Agent CLI.
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/entrypoint.sh"]
CMD ["gateway"]
