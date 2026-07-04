#!/usr/bin/env bash
# dev.sh — 本地开发：构建网关二进制并在前台运行。
#
# 用途：开发者本地调试网关。make build 编译二进制，然后直接运行，
# 监听 4096 端口。需要本地已装 Go（基础镜像里的工具链是为了容器，本地只需 Go）。
#
# 可选环境变量：
#   PORT   监听端口（默认 4096）
#   TOKEN  网关 Bearer token（默认空 = 不鉴权）
#   DATA_DIR  会话数据目录（默认 ~/.cc-connect）
#
# 用法：
#   ./scripts/dev.sh                 # 默认 4096 端口启动
#   PORT=8080 TOKEN=secret ./scripts/dev.sh
set -euo pipefail

cd "$(dirname "$0")/.."

PORT="${PORT:-4096}"
TOKEN="${TOKEN:-}"
DATA_DIR="${DATA_DIR:-}"

echo "==> 构建网关二进制"
make build

echo "==> 启动网关 (端口 $PORT)"
echo "    Token: ${TOKEN:-<无，开发模式不鉴权>}"
echo "    按 Ctrl+C 停止"
echo

ARGS=("-port" "$PORT")
[ -n "$TOKEN" ] && ARGS+=("-token" "$TOKEN")
[ -n "$DATA_DIR" ] && ARGS+=("-data-dir" "$DATA_DIR")

exec ./bin/acg "${ARGS[@]}"
