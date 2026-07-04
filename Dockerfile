# syntax=docker/dockerfile:1
#
# agent-cli-gateway 网关镜像
#
# 自带完整 agent 运行时（claude CLI + Node + Python + git），通过 HTTP/SSE
# 对外提供统一的 session/event API，供 f1-web 调用。
#
# 两段式构建，builder 和 runtime 共用预构建的 agent-gateway-base 基础镜像：
#   micr.cloud.mioffice.cn/agent-getaway/agent-gateway-base:go1.25-node22-py3
#
# 构建命令（本地 arm64 交叉构建 amd64 并推送）:
#   docker buildx build --platform linux/amd64 \
#     -t micr.cloud.mioffice.cn/agent-getaway/agent-gateway:<tag> \
#     --push .
#
# 部署时挂载（镜像内不含任何敏感信息）:
#   /data      ← 唯一持久化卷（文件挂载服务一个卷对应一个目录，故全部收敛到此）。
#                HOME=/data 指到这里，claude / nb CLI 的配置都落在 $HOME 下：
#     /data/.claude/settings.json  ← claude 的 provider 配置（env 块含 API key 等）。
#                                     ⚠️ claude CLI 读 $HOME/.claude/settings.json，
#                                     不是 CLAUDE_CONFIG_DIR（网关代码自定义变量，claude 不认）。
#                                     部署方把 settings.json 放进这个卷即可。
#     /data/.claude/projects/      ← claude 会话 transcript（*.jsonl），保证容器重启后
#                                     多轮 resume 不丢上下文。
#     /data/.nocobase/             ← nb CLI 的 env 配置。entrypoint 启动时执行
#                                     `nb init --setup-mode connect-remote --force`
#                                     连接已有 NocoBase 实例，配置写在此卷；
#                                     --force 保证幂等（已存在则重配）。
#     /data/workspace/             ← agent workDir（网关 cwd，读写）。
#
# 运行时环境变量（nb init 用，镜像内无敏感信息，由部署方注入）:
#   NB_ENV_NAME      nb env 名字（默认 default）
#   NB_API_BASE_URL  已有 NocoBase 的 API URL（含 /api，如 https://nocobase.internal/api）
#   NB_ACCESS_TOKEN  API key / access token
# 不传 → 跳过 nb init，镜像当纯网关用（向后兼容）。
#
# entrypoint.sh 在启动时幂等确保 nb init 完成，再 exec 启动网关（见文件底部）。

# ---- Stage 1: build Go binary ----
FROM micr.cloud.mioffice.cn/agent-getaway/agent-gateway-base:go1.25-node22-py3 AS builder
# 内网构建机访问不了 proxy.golang.org，走国内代理拉 Go 模块。
# 两个代理 fallback，任一可达即可。
ENV GOPROXY=https://goproxy.cn,https://mirrors.aliyun.com/goproxy/,direct
ENV GOSUMDB=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# cmd/gateway 的 plugin_agent_*.go 用 !no_<agent> tag 注册 agent，默认全编入；
# 网关无 web 依赖，无需任何 build tag（与原 Dockerfile 一致）。
RUN CGO_ENABLED=0 go build -o /out/acg ./cmd/gateway/

# ---- Stage 2: runtime (claude CLI + 工具链 + 二进制) ----
FROM micr.cloud.mioffice.cn/agent-getaway/agent-gateway-base:go1.25-node22-py3

# claude CLI —— npm 全局安装。
# 不能用官方 native 安装（curl claude.ai/install.sh）：claude.ai 在国内被地区屏蔽，
# 构建机拉到的会是 "App unavailable in region" HTML 而非脚本。npm registry 国内可达。
# 设国内镜像源加速；--omit=dev 跳过开发依赖缩小镜像。
RUN npm config set registry https://registry.npmmirror.com \
    && npm install -g @anthropic-ai/claude-code

# nb CLI（NocoBase CLI）—— 容器启动时 entrypoint 用它 `nb init` 连接已有 NocoBase 实例。
# 走公司内网 npm 源（@nocobase/cli 在公网 npmmirror 不一定收录；内网源可达）。
RUN npm install -g @nocobase/cli --registry=https://im-f1.test.mi.com/npm/

# license-kit 平台二进制变体（必需补装，否则 nb 命令报 MODULE_NOT_FOUND）。
# @nocobase/cli 依赖的 @nocobase/license-kit 是 napi-rs 平台包，运行时按 process.platform/arch
# require 对应变体。cli 通过 buildx 在 arm64 构建机上交叉构建时，npm 因该变体声明的
# os/cpu/libc 约束（linux/x64/glibc）与构建机（arm64）不匹配，用 notsup 拒装它，
# 导致容器（linux-x64）运行时 require 失败。--force 绕过平台检查强制装上目标变体。
# ⚠️ 这个变体内网源（im-f1.test.mi.com）没有，只有 npm 官方 registry 有，故不走内网源。
RUN npm install -g @nocobase/license-kit-linux-x64-gnu --force

# 网关二进制
COPY --from=builder /out/acg /usr/local/bin/acg

# HOME 指向可挂载卷 /data（/root 无法挂载）。claude CLI 的 .claude/（config + projects
# transcript）和 nb CLI 的 .nocobase/（env 配置）都硬编码落在 $HOME 下，把 HOME 设到
# /data 后两者自动写进同一个卷；workDir 锚点（网关 cwd）也挪到 /data/workspace 下，
# 这样「claude 配置 + nb 配置 + agent 工作目录」全部收敛到一个卷，挂 /data 即可。
ENV HOME=/data
RUN mkdir -p /data/workspace

# 预设 .claude.json 跳过 claude 首次 onboarding 检查。
# ⚠️ 关键：claude 首次启动若 hasCompletedOnboarding!=true，会忽略 ANTHROPIC_BASE_URL
# 直连 api.anthropic.com 做联网 onboarding 检查（github issue #26935），在内网容器里
# 会 ERR_BAD_REQUEST。预设此文件后 claude 直接读 settings.json 的 provider 配置。
# claude 读 $HOME/.claude.json（HOME=/data → /data/.claude.json），不是 .claude/ 文件夹里。
# 模板放镜像内固定路径（不在 /data 卷里，不会被挂载覆盖）；entrypoint 启动时若卷里
# 还没有 .claude.json 就拷过去——因为 /data 卷会盖掉镜像层，直接 COPY 到 /data 不可靠。
COPY docker/base/claude.json /usr/local/share/claude.onboarding-skip.json

# workDir 根 = 网关进程 cwd（server/session_store.go 的 resolveWorkDir 以 os.Getwd() 为锚）。
# 第一步：单目录；第二步：按用户隔离成 /data/workspace/users/<uid>/project。
# 放在 HOME=/data 下，与 .claude / .nocobase 共享同一个挂载卷。
WORKDIR /data/workspace

EXPOSE 4096

# 健康检查：/health 不需要认证，返回 agent 列表。
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsS http://localhost:4096/health || exit 1

# entrypoint 包装脚本：启动时幂等执行 nb init（connect-remote，连接已有 NocoBase），
# 完成后 exec 启动网关。配置由 NB_* 环境变量注入，未配置则跳过 nb init 直接启网关。
COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["acg", "-port", "4096"]
