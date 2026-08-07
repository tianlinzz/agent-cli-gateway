# Agent Gateway Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 重写项目为一个只暴露 OpenAI-compatible `/v1/models` 和 `/v1/chat/completions` 的云端 Agent Gateway，第一阶段支持 Claude Code、Codex 和 Kimi，并在一个容器内以独立 Worker 进程执行 Agent CLI。

**Architecture:** API Gateway 负责 HTTP、鉴权、model 路由、session 元数据和 OpenAI SSE；Runtime 通过稳定的 execution contract 管理 Worker；Worker Supervisor 启动本地 Worker 进程；每个 Worker 只承载一个 AgentSession 和一个 CLI 进程；三个 Agent 通过独立 adapter 接入。未来跨节点只替换 execution backend，不改变 API 和 adapter contract。

**Tech Stack:** Go、标准 `net/http`、gRPC（本地进程 RPC contract）、TOML 配置、现有 Agent CLI 协议解析代码、Go test/race。

---

## 文件结构

新代码集中在以下目录，旧 `core/`、旧 `server/` 和旧入口在迁移完成后删除：

```text
api/openai/                 OpenAI request/response、SSE、错误
runtime/                    Agent registry、session、canonical events
worker/                     Worker RPC、supervisor、本地进程 backend
adapters/claudecode/        Claude Code adapter
adapters/codex/             Codex adapter
adapters/kimi/              Kimi adapter
config/                     新 gateway TOML 配置
workspace/                  workspace_id 到受控目录的解析和校验
cmd/gateway/                新入口和插件注册
```

迁移来源仅限：`agent/claudecode`、`agent/codex`、`agent/kimi` 中已验证的 CLI 进程、原生协议、resume、权限和 usage 代码；不迁移 IM、Platform、旧 Message、cron/timer/relay 和管理端。

### Task 1: 建立新模块入口和空架构

**Files:**
- Create: `runtime/contract.go`
- Create: `runtime/registry.go`
- Create: `runtime/events.go`
- Create: `config/gateway.go`
- Create: `cmd/gateway/main.go`
- Test: `runtime/registry_test.go`

- [ ] **Step 1: 写 registry 失败测试**

测试 `Register`、`Resolve`、重复注册和未知 model 错误；测试注册项只允许 `codex`、`claude-code`、`kimi` 的 adapter 在后续接入，registry 本身不硬编码名称。

- [ ] **Step 2: 运行 `go test ./runtime -run TestRegistry -v`，确认新包尚不存在而失败**

- [ ] **Step 3: 定义 canonical contract**

在 `runtime/contract.go` 定义 `Descriptor`、`Capabilities`、`StartRequest`、`Input`、`Message`、`Tool`、`AgentAdapter`、`Session` 和 `ExecutionBackend`。这些类型不得导入 `net/http` 或任何具体 adapter。

- [ ] **Step 4: 实现线程安全 registry**

`runtime/registry.go` 使用 mutex 保护 adapter map；`List` 排序返回稳定结果；`Resolve` 返回带可用 model 列表的错误。

- [ ] **Step 5: 添加默认配置解析**

`config/gateway.go` 定义 server、auth、workspace 和 agent command/default model/permission/timeout 字段，并给出三个 Agent 的 enabled 配置默认值；不允许把客户端的绝对 `workDir` 直接带入运行时。

- [ ] **Step 6: 运行 `gofmt -w runtime config cmd/gateway` 和 `go test ./runtime ./config`**

- [ ] **Step 7: 提交**

```bash
git add runtime config/gateway.go cmd/gateway/main.go
git commit -m "feat: add agent gateway runtime contracts"
```

### Task 2: 实现 workspace resolver 和 session metadata

**Files:**
- Create: `workspace/resolver.go`
- Create: `workspace/resolver_test.go`
- Create: `runtime/session.go`
- Create: `runtime/session_store.go`
- Create: `runtime/session_store_test.go`

- [ ] **Step 1: 写 workspace 安全回归测试**

覆盖合法 `workspace_id`、路径穿越、空 ID、根目录不存在和 owner 不匹配；断言解析结果始终位于配置的 workspace root 内。

- [ ] **Step 2: 运行 `go test ./workspace -v`，确认测试失败**

- [ ] **Step 3: 实现 `workspace.Resolver`**

只接受 opaque `workspace_id`，使用 `filepath.Rel` 校验最终目录没有逃逸 root；按需创建目录，并返回绝对受控路径。

- [ ] **Step 4: 写 session store 回归测试**

覆盖创建、按 owner 读取、重复 active turn 拒绝、touch、过期和删除；测试 session metadata 不保存 Go process 指针。

- [ ] **Step 5: 实现内存 session store**

定义 `SessionRecord` 和 `SessionStatus`；用 mutex 保护 map；所有读取返回副本；为未来 `PersistentSessionStore` 保留接口，不引入 Redis 依赖。

- [ ] **Step 6: 运行 `go test ./workspace ./runtime -race`**

- [ ] **Step 7: 提交**

```bash
git add workspace runtime/session.go runtime/session_store.go
git commit -m "feat: add isolated workspace and session metadata"
```

### Task 3: 实现 Worker RPC contract 和 supervisor

**Files:**
- Create: `worker/proto/worker.proto`
- Create: `worker/rpc.go`
- Create: `worker/supervisor.go`
- Create: `worker/local_backend.go`
- Create: `worker/supervisor_test.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 写 supervisor 生命周期测试**

使用假的 worker executable，覆盖启动、握手失败、事件流断开、abort、超时、SIGTERM 和子进程回收；测试 worker 异常不会让 API 主进程退出。

- [ ] **Step 2: 运行 `go test ./worker -run TestSupervisor -v`，确认测试失败**

- [ ] **Step 3: 定义 RPC 消息**

在 proto 中定义 `StartSession`、`SendInput`、`StreamEvents`、`AbortSession`、`CloseSession` 和 `Health`；消息只使用 canonical runtime 类型对应的字段，不包含 OpenAI JSON 或具体 Agent 字段。

- [ ] **Step 4: 生成 gRPC 代码并实现本地 client/server**

`worker/rpc.go` 负责消息编码和 canonical event 转换；Unix socket 用于本地通信，抽象为 endpoint，不能让 runtime 依赖 socket 细节。

- [ ] **Step 5: 实现 supervisor**

Supervisor 为一个 session 启动一个 worker 子进程，记录 worker ID/PID，监听退出，发送取消，等待进程组退出，超时后强杀整个进程组并回收资源。

- [ ] **Step 6: 实现 `LocalExecutionBackend`**

将 runtime `StartRequest` 转成 worker RPC 请求，返回 `ExecutionHandle`；API 层只能通过此接口执行。

- [ ] **Step 7: 运行 `go test ./worker -race` 和 `go vet ./worker`**

- [ ] **Step 8: 提交**

```bash
git add worker go.mod go.sum
git commit -m "feat: add process-isolated worker backend"
```

### Task 4: 接入 Codex adapter，完成第一个垂直闭环

**Files:**
- Create: `adapters/codex/adapter.go`
- Create: `adapters/codex/session.go`
- Create: `adapters/codex/protocol.go`
- Create: `adapters/codex/adapter_test.go`
- Modify: `cmd/gateway/plugin_agent_codex.go`

- [ ] **Step 1: 从现有 `agent/codex` 选择协议处理代码并写 adapter regression tests**

测试启动参数、输入转换、JSON-RPC/stream 事件、native session ID、usage、permission、abort 和异常退出；测试不得依赖真实 Codex 登录态。

- [ ] **Step 2: 运行 `go test ./adapters/codex -v`，确认新 adapter 测试失败**

- [ ] **Step 3: 实现 `CodexAdapter.Describe`**

检查 command 可执行性并返回 model ID `codex`、能力 metadata 和配置中的 runtime model 列表。

- [ ] **Step 4: 实现 `CodexAdapter.Start`**

只构造 Codex 原生启动请求和 session，不暴露 HTTP 类型；所有 CLI 进程由 worker 进程创建和回收。

- [ ] **Step 5: 运行 adapter 测试和 `go test ./worker ./runtime`**

- [ ] **Step 6: 提交**

```bash
git add adapters/codex cmd/gateway/plugin_agent_codex.go
git commit -m "feat: connect codex through agent adapter"
```

### Task 5: 实现 OpenAI model discovery 和 Chat Completions

**Files:**
- Create: `api/openai/types.go`
- Create: `api/openai/models.go`
- Create: `api/openai/chat_completions.go`
- Create: `api/openai/stream.go`
- Create: `api/openai/errors.go`
- Create: `api/openai/handlers_test.go`
- Modify: `cmd/gateway/main.go`

- [ ] **Step 1: 写 OpenAI API 失败测试**

覆盖 `/v1/models` 稳定排序、未知 model、非流式 completion、流式 chunk、`[DONE]`、鉴权失败、owner session 隔离、client disconnect 和 abort。

- [ ] **Step 2: 运行 `go test ./api/openai -v`，确认测试失败**

- [ ] **Step 3: 实现 `/v1/models`**

从 runtime registry 动态生成 OpenAI model list；只返回启用且 discovery 成功的 Agent。

- [ ] **Step 4: 实现请求 normalizer**

把 OpenAI messages、tools、metadata、session header 和 workspace ID 转成 `runtime.Input`/`StartRequest`；禁止客户端直接注入绝对 workdir。

- [ ] **Step 5: 实现非流式响应**

消费 canonical events，聚合 text、tool metadata、usage 和 finish reason，输出 OpenAI completion JSON；agent error 使用标准 OpenAI error envelope。

- [ ] **Step 6: 实现流式 SSE**

将 canonical text/tool/usage/error 事件转换为 OpenAI chunks；正确设置 `text/event-stream`、flush、request cancellation 和最后的 `data: [DONE]`。

- [ ] **Step 7: 接入 `main.go`**

加载 config、创建 registry、启动 supervisor/backend、注册 HTTP routes 和 graceful shutdown；API 进程不得导入具体 agent CLI 包之外的执行实现。

- [ ] **Step 8: 运行 `go test ./api/openai ./runtime ./worker -race`**

- [ ] **Step 9: 提交**

```bash
git add api/openai cmd/gateway/main.go
git commit -m "feat: expose openai compatible agent gateway"
```

### Task 6: 接入 Claude Code 和 Kimi

**Files:**
- Create: `adapters/claudecode/adapter.go`
- Create: `adapters/claudecode/session.go`
- Create: `adapters/claudecode/protocol.go`
- Create: `adapters/claudecode/adapter_test.go`
- Create: `adapters/kimi/adapter.go`
- Create: `adapters/kimi/session.go`
- Create: `adapters/kimi/protocol.go`
- Create: `adapters/kimi/adapter_test.go`
- Modify: `cmd/gateway/plugin_agent_claudecode.go`, `cmd/gateway/plugin_agent_kimi.go`

- [ ] **Step 1: 为 Claude Code 写失败测试并运行**

覆盖 stream-json、resume、permission、tool event、usage、abort 和 CLI failure。

- [ ] **Step 2: 实现 Claude Code adapter 并运行 `go test ./adapters/claudecode -race`**

- [ ] **Step 3: 为 Kimi 写失败测试并运行**

覆盖 Kimi 原生输出、session resume、usage、abort 和异常退出。

- [ ] **Step 4: 实现 Kimi adapter 并运行 `go test ./adapters/kimi -race`**

- [ ] **Step 5: 添加三 Agent discovery 和 Chat Completions 集成测试**

使用 fake adapters 验证同一个 OpenAI API 根据 `model` 路由到三个不同 adapter，并断言用户看到的响应而不是内部 session 字段。

- [ ] **Step 6: 提交**

```bash
git add adapters cmd/gateway
git commit -m "feat: add claude code and kimi adapters"
```

### Task 7: 删除旧实现并完成容器入口

**Files:**
- Delete: 旧 `core/` 中 Platform、Message、IM prompt、cron/timer/relay 相关文件
- Delete: 旧 `server/` HTTP/session API 文件
- Modify: `cmd/gateway/main.go`, `Dockerfile`, `docker/entrypoint.sh`, `README.md`, `Makefile`
- Test: `integration/agent_gateway_test.go`

- [ ] **Step 1: 先用 `rg` 建立旧依赖清单**

确认新代码不再导入 Platform、MessageHandler、ReplyCtx、旧 `/session`、旧 `/event` 和 IM-only symbols。

- [ ] **Step 2: 删除旧 HTTP、IM 和管理面代码**

只删除已由新 runtime/API 替代的代码；保留尚未迁移但属于三种 Agent 协议实现的源文件直到 adapter 测试完全覆盖。

- [ ] **Step 3: 更新 Docker 入口**

容器以 supervisor 作为 PID 1，API 和 Worker 进程共享配置目录和受控 workspace root；SIGTERM 必须按 API -> worker -> CLI 的顺序传播并等待回收。

- [ ] **Step 4: 写容器级集成测试**

启动 fake CLI worker，验证 API health、model discovery、stream completion、abort、worker crash 后 API 仍可用和 workspace 越界被拒绝。

- [ ] **Step 5: 运行最终验证**

```bash
gofmt -w api runtime worker adapters config cmd/gateway
go vet ./...
go test ./...
go test -race ./...
go build ./cmd/gateway
```

- [ ] **Step 6: 提交**

```bash
git add -A
git commit -m "refactor: rebuild gateway around isolated agent workers"
```

## 交付顺序

每个 Task 都应保持可编译、可测试并单独提交。实现时先完成 Task 1-3，再以 Codex 作为第一个垂直闭环完成 Task 4-5；Claude Code 和 Kimi 接入后再执行旧代码删除和容器收口。

