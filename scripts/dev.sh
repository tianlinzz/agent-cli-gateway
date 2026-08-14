#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
source ./scripts/dev-agents.sh

PORT="${PORT:-4096}"
DEV_DIR="${DEV_DIR:-$PWD/.gateway-dev}"
WORKSPACE_ROOT="${WORKSPACE_ROOT:-$DEV_DIR/workspaces}"
RUNTIME_DIR="${RUNTIME_DIR:-$DEV_DIR/runtime}"
CONFIG="${GATEWAY_CONFIG:-$DEV_DIR/gateway.toml}"
WORKER_EXEC="${GW_WORKER_EXEC:-$PWD/bin/gateway-worker}"
mkdir -p "$DEV_DIR" "$WORKSPACE_ROOT" "$RUNTIME_DIR" bin

VERSION="${VERSION:-dev}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildTime=${BUILD_TIME}"
echo "==> 构建 Gateway"
CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o bin/gateway ./cmd/gateway
echo "==> 构建 Gateway Worker"
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/gateway-worker ./cmd/gateway-worker

TOKEN_FILE="$DEV_DIR/dev-token"
if [[ -n "${DEV_TOKEN:-}" ]]; then
  printf '%s' "$DEV_TOKEN" > "$TOKEN_FILE"
elif [[ ! -s "$TOKEN_FILE" ]]; then
  openssl rand -hex 24 > "$TOKEN_FILE"
fi
chmod 600 "$TOKEN_FILE"
TOKEN="$(<"$TOKEN_FILE")"

CLAUDE_COMMAND="${CLAUDE_COMMAND:-claude}"
CODEX_COMMAND="${CODEX_COMMAND:-codex}"
KIMI_COMMAND="${KIMI_COMMAND:-kimi}"
CLAUDE_ENABLED="$(dev_agent_enabled "${DEV_ENABLE_CLAUDE:-auto}" "$CLAUDE_COMMAND")"
CODEX_ENABLED="$(dev_agent_enabled "${DEV_ENABLE_CODEX:-auto}" "$CODEX_COMMAND")"
KIMI_ENABLED="$(dev_agent_enabled "${DEV_ENABLE_KIMI:-auto}" "$KIMI_COMMAND")"

OS="$(uname -s)"
if [[ "$OS" == "Linux" && "${DEV_ISOLATION:-on}" != "off" ]]; then
  MODE="dev"; REQUIRED="true"; SECCOMP="kafel"; WORKER_MODE="nsjail"
else
  MODE="test"; REQUIRED="false"; SECCOMP="off"; WORKER_MODE="direct"
fi

toml_quote() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }
cat > "$CONFIG" <<EOF
mode = "$MODE"

[server]
listen_addr = "127.0.0.1:$PORT"
drain_timeout = "30s"

[worker]
stop_grace_period = "10s"
heartbeat_interval = "15s"
heartbeat_timeout = "3s"
heartbeat_failures = 3

[sessions]
idle_timeout = "2h"
reap_interval = "1m"

[auth]
required = true
callers = [{ id = "local-dev", tokens = ["$(toml_quote "$TOKEN")"] }]

[workspace]
root = "$(toml_quote "$WORKSPACE_ROOT")"

[isolation]
required = $REQUIRED
nsjail_version = "3.6"
nsjail_source = "https://github.com/google/nsjail"
binary_path = "/usr/local/bin/nsjail"

[isolation.seccomp]
policy = "$SECCOMP"

[agents.codex]
enabled = $CODEX_ENABLED
command = ["$(toml_quote "$CODEX_COMMAND")"]
permission = "auto"

[agents.claude-code]
enabled = $CLAUDE_ENABLED
command = ["$(toml_quote "$CLAUDE_COMMAND")"]
permission = "auto"
default_model = "sonnet"
models = ["sonnet", "opus", "haiku"]

[agents.kimi]
enabled = $KIMI_ENABLED
command = ["$(toml_quote "$KIMI_COMMAND")"]
permission = "auto"
timeout = "30m"
models = ["kimi-k3", "deepseek-v4-flash"]
EOF

echo "==> 启动 Gateway"
echo "    地址: http://127.0.0.1:$PORT"
echo "    Worker: $WORKER_MODE"
if [[ "$REQUIRED" == true ]]; then echo "    nsjail: enabled"; else echo "    nsjail: disabled (local development only)"; fi
echo "    Token 文件: $TOKEN_FILE"
echo "    配置文件: $CONFIG"
echo "    Agents: claude-code=$CLAUDE_ENABLED codex=$CODEX_ENABLED kimi=$KIMI_ENABLED"
echo "    按 Ctrl+C 停止"
export GATEWAY_RUNTIME_DIR="$RUNTIME_DIR"
exec "$PWD/bin/gateway" -config "$CONFIG" -worker-exec "$WORKER_EXEC"
