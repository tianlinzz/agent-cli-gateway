#!/bin/sh
#
# 网关启动入口（在 acg 之前跑）。
#
# 职责：幂等地确保 nb init 已完成（connect-remote，连接已有 NocoBase 实例），
# 然后 exec 启动网关二进制。
#
# 配置由运行时环境变量注入，镜像内不含任何 URL/token：
#   NB_ENV_NAME      nb env 名字（默认 default）
#   NB_API_BASE_URL  已有 NocoBase 的 API URL（含 /api，如 https://nocobase.internal/api）
#   NB_ACCESS_TOKEN  API key / access token
#
# 不传 NB_API_BASE_URL / NB_ACCESS_TOKEN → 跳过 nb init，镜像当纯网关用（向后兼容）。
# 幂等：--force 让 nb init 在 env 已存在时重新配置（connect-remote 只存 API URL+token，
#   无 DB/服务可冲突），故每次启动都安全；不依赖脆弱的 nb env list 表格解析。
set -e

NB_ENV="${NB_ENV_NAME:-default}"
NB_API_URL="${NB_API_BASE_URL:-}"
NB_TOKEN="${NB_ACCESS_TOKEN:-}"

# /data 是运行时挂载的持久卷，会盖掉镜像层。卷里首次起是空的，需补回两样镜像预置物：
#   1) .claude.json —— 跳过 claude 首次 onboarding 联网检查（issue #26935）。
#      模板存在镜像内 /usr/local/share/claude.onboarding-skip.json（不在卷里，不会被盖）。
#   2) workspace/   —— workDir 根（网关 cwd）。缺失则建。
# 幂等：卷里已有同名文件/目录则保留，不覆盖。
if [ ! -f "$HOME/.claude.json" ] && [ -f /usr/local/share/claude.onboarding-skip.json ]; then
  cp /usr/local/share/claude.onboarding-skip.json "$HOME/.claude.json"
fi
mkdir -p "$HOME/workspace"

if [ -n "$NB_API_URL" ] && [ -n "$NB_TOKEN" ]; then
  echo "[entrypoint] running nb init (connect-remote) for env '${NB_ENV}'"
  nb init \
    --env "$NB_ENV" \
    --yes \
    --force \
    --setup-mode connect-remote \
    --api-base-url "$NB_API_URL" \
    --auth-type token \
    --access-token "$NB_TOKEN" \
    --skip-skills
else
  echo "[entrypoint] NB_API_BASE_URL / NB_ACCESS_TOKEN not set; skipping nb init"
fi

# 启动网关（CMD 透传到这里），替换当前进程，让 acg 成为 PID 1 接收信号。
exec "$@"
