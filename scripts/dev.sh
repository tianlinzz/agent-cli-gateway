#!/usr/bin/env bash
# dev.sh — 本地开发：构建网关二进制并在前台运行。
#
# 用途：开发者本地调试网关。编译二进制，然后直接运行，
# 监听 4096 端口。需要本地已装 Go（不需要 make）。
#
# 可选环境变量：
#   PORT   监听端口（默认 4096）
#   TOKEN  网关 Bearer token（默认空 = 不鉴权）
#   DATA_DIR  会话数据目录（默认 ~/.cc-connect）
#
# 用法：
#   ./scripts/dev.sh                 # 默认 4096 端口启动
#   PORT=8080 TOKEN=secret ./scripts/dev.sh
#
# 跨平台：在 Linux/macOS/Windows(Git Bash) 下均可运行。Windows 下自动
# 生成并执行带 .exe 后缀的二进制，不依赖 make。
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-4096}"
TOKEN="${TOKEN:-}"
DATA_DIR="${DATA_DIR:-}"

# --- 选择目标二进制名（Windows/Git Bash 需要 .exe 后缀） ---------------
APP="acg"
BIN="bin/${APP}"
case "$(uname -s)" in
  MINGW*|MSYS*|CYGWIN*) BIN="${BIN}.exe" ;;
esac

# --- 版本信息注入（与 Makefile 保持一致） ------------------------------
VERSION="${VERSION:-dev}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo "none")"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"

echo "==> 构建网关二进制 (${BIN})"
CGO_ENABLED=0 go build -ldflags "${LDFLAGS}" -o "${BIN}" ./cmd/gateway

echo "==> 启动网关 (端口 ${PORT})"
echo "    Token: ${TOKEN:-<无，开发模式不鉴权>}"
echo "    按 Ctrl+C 停止"
echo

ARGS=("-port" "${PORT}")
[ -n "${TOKEN}" ]    && ARGS+=("-token" "${TOKEN}")
[ -n "${DATA_DIR}" ] && ARGS+=("-data-dir" "${DATA_DIR}")

exec "${BIN}" "${ARGS[@]}"
