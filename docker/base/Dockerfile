# agent-cli-gateway base image: Go + Node + Python + git
#
# 供网关 Dockerfile 的 builder 和 runtime 两个 stage 共用。
# 预构建并推送到 micr.cloud.mioffice.cn/agent-getaway/agent-gateway-base
#
# 基于 debian slim（不用 alpine）：claude CLI 是 Node 包，agent 运行时调
# python/git，debian 的 glibc + apt 工具链兼容性最稳。
#
# 构建命令（本地 arm64 交叉构建 amd64 并推送）:
#   docker buildx build --platform linux/amd64 \
#     -t micr.cloud.mioffice.cn/agent-getaway/agent-gateway-base:go1.25-node22-py3 \
#     --push -f docker/base/Dockerfile docker/base

FROM debian:bookworm-slim

# 一次性装齐：构建工具、git、Python3（带 pip + venv）、ca-certificates。
# 这些是 claude agent 在 workDir 里干活时会调用的工具链。
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        git \
        build-essential \
        python3 \
        python3-pip \
        python3-venv \
    && rm -rf /var/lib/apt/lists/*

# Node.js 22 LTS — claude CLI 的运行时（claude CLI 要求 Node >= 18）
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Go 1.25 — 网关二进制编译用（builder stage 需要）
ARG GO_VERSION=1.25.0
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    | tar -C /usr/local -xz
ENV GOROOT=/usr/local/go
ENV GOPATH=/go
ENV PATH="${GOROOT}/bin:${GOPATH}/bin:${PATH}"

# 验证工具链齐全（构建时即失败，避免推了残缺镜像）
RUN set -e; \
    echo "go:       $(go version)"; \
    echo "node:     $(node --version)"; \
    echo "npm:      $(npm --version)"; \
    echo "python3:  $(python3 --version)"; \
    echo "pip3:     $(pip3 --version)"; \
    echo "git:      $(git --version)"
