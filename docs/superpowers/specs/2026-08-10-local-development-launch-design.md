# 本地开发 Worker 启动设计

## 目标

在 macOS 上保留生产架构的 API Gateway → Worker Supervisor → 独立
`gateway-worker` → Agent CLI 执行链路。开发脚本负责构建两个二进制、生成
本地配置和目录，并启动 Gateway；开发者不需要手动启动 Worker。

## 非目标

- 不修改生产模式的 nsjail fail-closed 安全边界。
- 不在 API 进程中直接启动 Claude Code、Codex 或 Kimi CLI。
- 不把 macOS 的无 sandbox 行为伪装成生产隔离。
- 不引入管理端、持久化用户系统或新的运行时部署模式。

## 运行策略

### macOS / Windows 本地开发

脚本生成临时的本地开发配置：

```toml
mode = "test"

[auth]
required = true

[isolation]
required = false

[isolation.seccomp]
policy = "off"
```

`mode=test + isolation.required=false` 是当前代码允许的无 nsjail 直接 Worker
路径。这个配置只由开发脚本生成，启动日志必须明确说明 `worker mode: direct`
以及 `nsjail: disabled (local development only)`。

### Linux 本地开发

默认仍生成 `mode=dev`、`isolation.required=true`，使用真实 nsjail。若开发者
明确设置 `DEV_ISOLATION=off`，脚本可以生成直接 Worker 配置，但必须打印强警告；
生产配置和生产启动入口始终不能关闭 nsjail。

## 脚本行为

`scripts/dev.sh` 和 `scripts/dev.ps1` 保持等价：

1. 从仓库根目录运行。
2. 构建 `bin/gateway` 和 `bin/gateway-worker`，使用已有版本、commit、build time
   注入方式。
3. 创建 `.gateway-dev/workspaces` 和 `.gateway-dev/runtime`，目录权限为当前用户
   可读写。
4. 生成 `.gateway-dev/gateway.toml`，使用绝对路径，避免当前工作目录变化导致
   Supervisor 或 workspace 解析错误。
5. 配置 `worker_exec` 指向刚构建的 `bin/gateway-worker`。
6. 默认监听 `127.0.0.1:4096`；支持 `PORT`、`DEV_TOKEN`、`WORKSPACE_ROOT`、
   `RUNTIME_DIR` 和 Linux-only `DEV_ISOLATION` 环境变量。
7. 不打印 token 明文；请求示例从脚本输出的 token 文件读取。
8. 使用 `exec`（PowerShell 中直接等待子进程）启动 Gateway，并把 Ctrl-C 传递给
   Gateway 的优雅关闭流程。

默认开发 token 写入 `.gateway-dev/dev-token`，权限为 `0600`；如果用户提供
`DEV_TOKEN`，脚本只写入该文件，不在 stdout 打印值。

## 配置生成

脚本生成的 Agent 配置只包含当前支持的三个 Agent：

- `codex`
- `claude-code`
- `kimi`

Agent CLI command 默认从 PATH 解析，也允许通过 `CODEX_COMMAND`、
`CLAUDE_CODE_COMMAND`、`KIMI_COMMAND` 覆盖。Provider API key 不写进 TOML；
沿用当前进程环境传给 Worker 的方式。

## 安全边界

- 生产 `mode=prod` 和 Linux 默认 `mode=dev` 仍要求 nsjail。
- 本地脚本生成的 `mode=test` 只是明确的无 sandbox 开发配置。
- workspace 仍通过 caller-scoped resolver 解析，不能由请求注入绝对路径。
- 每个 session 仍由 Supervisor 创建独立 Worker、socket、agent home 和运行目录。
- 同一个 session 仍只能有一个 active turn；不同 session 可以并发。

## 验证

脚本改造完成后必须验证：

- `./scripts/dev.sh --help` 或等价帮助输出可用。
- macOS 下配置生成成功，`go build` 成功，Gateway 可启动并报告 direct Worker。
- `/health/live` 可访问，`/health/ready` 正确反映 isolation disabled。
- 使用 hermetic test worker 或 fake Agent CLI 验证一次真实 Worker session。
- `go test ./...`、`go vet ./...`、`go build ./...` 通过。
- Linux CI 保持 nsjail smoke 和生产 fail-closed 行为不变。
