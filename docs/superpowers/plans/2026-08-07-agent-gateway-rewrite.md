# Agent Gateway Rewrite Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 重写项目为一个只暴露 OpenAI-compatible `/v1/models` 和 `/v1/chat/completions` 的云端 Agent Gateway，第一阶段支持 Claude Code、Codex 和 Kimi，并在一个容器内以独立 Worker 进程执行 Agent CLI。

**Architecture:** API Gateway 负责 HTTP、鉴权、model 路由、session 元数据和 OpenAI SSE；Runtime 通过稳定的 execution contract 管理 Worker；Worker Supervisor 启动本地 Worker 进程；每个 Worker 只承载一个 AgentSession 和一个 CLI 进程，Worker 与 CLI 整体由 **nsjail 沙箱**（mount namespace、受控 workspace、seccomp-bpf + rlimit）包裹；三个 Agent 通过独立 adapter 接入。未来跨节点只替换 execution backend，不改变 API 和 adapter contract。

**Tech Stack:** Go、标准 `net/http`、gRPC（本地进程 RPC contract）、TOML 配置、**nsjail**（C++ 隔离二进制，Linux namespace/cgroup/seccomp）、Go test/race。

> **决策对齐（2026-08-07，补充计划/spec 未明确的项）：**
> 1. **IPC** = gRPC + protoc over per-session Unix sockets（设计文档「gRPC or Unix socket」二选一，采用 gRPC contract 和 Unix socket transport）。
> 2. **权限** = permission 是 canonical runtime contract 的一部分；部署配置可选择 `auto`、`ask` 或 `deny`。第一阶段可以默认 `auto`，但不能删除 permission event，也不能把 nsjail 当作自动批准的替代品。
> 3. **adapter** = 全新实现，仅把现有 `agent/{codex,claudecode,kimi}/` 当协议参考，零代码移植。
> 4. **nsjail 纳入第一阶段**，包裹 worker fork 出的每一个 Worker 及其 Agent CLI（补计划 Task 3 原本「仅进程组回收」的隔离缺口）。
> 5. **nsjail 运行模型**：(a) workspace = bind-mount 真实受控目录进沙箱 `/workspace`（非 tmpfs，保证 resume/多轮持久化）；(b) Agent home = 当前 Agent/session 专属可写目录 `/agent-home`；(c) `/tmp` 为 session 独立 tmpfs；(d) user-namespace 无特权降权，不加 CAP_SYS_ADMIN、不 privileged；(e) 第一阶段默认不创建独立 network namespace，保持 provider 网络可用；(f) Worker adapter 声明 `persistent_process` 或 `resume_per_turn`，不能把 `-Mo` 固定为所有 Agent 的 session 语义。
> 6. **nsjail 二进制** = Dockerfile 多阶段构建并固定上游 tag/commit；builder 安装 autoconf/bison/flex/libprotobuf/libnl，运行镜像复制二进制及其实际运行时依赖，并在 CI 验证 `nsjail --version`、`ldd` 和最小 smoke jail。

---

## 文件结构

新代码集中在以下目录，旧 `core/`、旧 `server/` 和旧入口在迁移完成后删除：

```text
api/openai/                 OpenAI request/response、SSE、错误
runtime/                    Agent registry、session、canonical events
worker/                     Worker RPC、supervisor、本地进程 backend、nsjail 集成
worker/nsjail/              nsjail profile 模板（Kafel seccomp + mount + rlimit）
adapters/claudecode/        Claude Code adapter（全新实现）
adapters/codex/             Codex adapter（全新实现）
adapters/kimi/              Kimi adapter（全新实现）
config/                     新 gateway TOML 配置（含 isolation 段）
workspace/                  workspace_id 到受控目录的解析和校验
cmd/gateway/                新入口和插件注册
```

adapters 为全新实现，仅把现有 `agent/claudecode`、`agent/codex`、`agent/kimi` 当作协议参考（CLI 启动、原生流解析、resume、permission、usage 的格式），不移植代码。不迁移 IM、Platform、旧 Message、cron/timer/relay 和管理端。

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

`config/gateway.go` 定义 server、auth、workspace 和 agent command/default model/permission/timeout 字段，并给出三个 Agent 的 enabled 配置默认值；不允许把客户端的绝对 `workDir` 直接带入运行时。同时定义 `Isolation` 配置段：nsjail 必须启用、二进制路径、profile 覆盖路径、workspace/agent-home/tmp mount 设置、rlimit/uid-gid 映射、seccomp policy 和 network namespace 开关。生产默认 `required=true`，本地测试只有显式配置才能禁用。

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
- Create: `worker/nsjail/profile.go`（nsjail profile 构建：mount bind workspace、seccomp Kafel 白名单、rlimit、user-namespace uid/gid 映射）
- Create: `worker/nsjail/profile_test.go`
- Create: `worker/supervisor_test.go`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 写 supervisor 生命周期测试**

使用假的 worker executable，覆盖启动、握手失败、事件流断开、abort、超时、SIGTERM 和子进程回收；额外覆盖「nsjail 包裹层挂掉时整组被回收」和「nsjail 配置缺失/二进制不可执行时 fail-closed/readiness 失败」（不要求单元测试环境真实装有 nsjail，用 stub binary 模拟）。测试 worker 异常不会让 API 主进程退出。

- [ ] **Step 2: 运行 `go test ./worker -run TestSupervisor -v`，确认测试失败**

- [ ] **Step 3: 定义 RPC 消息**

在 proto 中定义 `StartSession`、`SendInput`、`StreamEvents`、`AbortSession`、`CloseSession` 和 `Health`；消息只使用 canonical runtime 类型对应的字段，不包含 OpenAI JSON 或具体 Agent 字段。nsjail 是 worker 进程启动细节，**不进 proto**——profile 由 worker 侧从 config + workspace 路径本地组装。

- [ ] **Step 4: 生成 gRPC 代码并实现本地 client/server**

`worker/rpc.go` 负责消息编码和 canonical event 转换；gRPC 使用每 session 独立 Unix socket，抽象为 endpoint，不能让 runtime 依赖 socket 细节。socket 目录只向对应 nsjail 暴露。

- [ ] **Step 5: 实现 supervisor（含 nsjail 包裹）**

Supervisor 为一个 session 启动一个 worker 子进程：生产 isolation 必须启用，fork 的是 `nsjail -Mo --config <profile> -- <worker_executable>` 或 adapter 声明的等价 session 模式。profile 将真实受控 workspace bind-mount 到 `/workspace`，将 Agent 专属可写 home 挂载到 `/agent-home`，将 session tmpfs 挂载到 `/tmp`，并使用 user namespace、rlimit 和基础 seccomp policy；第一阶段默认不创建独立 network namespace。记录 worker ID/PID 与 nsjail PID，监听退出，发送取消，等待进程组退出，超时后强杀整个进程组（含 nsjail 与 Agent CLI）并回收资源。只有显式 local-dev 配置才允许关闭 isolation；生产配置缺失或启动失败必须 fail-closed。

- [ ] **Step 6: 实现 `LocalExecutionBackend`**

将 runtime `StartRequest` 转成 worker RPC 请求，返回 `ExecutionHandle`；API 层只能通过此接口执行。

- [ ] **Step 7: 运行 `go test ./worker ./worker/nsjail -race` 和 `go vet ./worker ./worker/nsjail`**

- [ ] **Step 8: 添加 nsjail preflight 和 profile smoke test**

启动时检查二进制版本、user namespace、mount namespace、seccomp 和最小 jail；失败时 readiness 返回 503，不得自动直接 fork worker。使用 fake CLI 验证 `/workspace` 可写、workspace root 外路径不可见、`/agent-home` 与其他 session 隔离，并验证 provider 网络在默认配置下可访问。

- [ ] **Step 9: 提交**

```bash
git add worker go.mod go.sum
git commit -m "feat: add process-isolated worker backend (nsjail-sandboxed)"
```

### Task 4: 接入 Codex adapter，完成第一个垂直闭环

**Files:**
- Create: `adapters/codex/adapter.go`
- Create: `adapters/codex/session.go`
- Create: `adapters/codex/protocol.go`
- Create: `adapters/codex/adapter_test.go`
- Modify: `cmd/gateway/plugin_agent_codex.go`

- [ ] **Step 1: 参考 `agent/codex` 协议写 adapter regression tests**

仅把现有 `agent/codex` 当协议参考（不移植代码），为全新 `adapters/codex` 写测试：启动参数、输入转换、JSON-RPC/stream 事件、native session ID、usage、permission、abort 和异常退出；测试不得依赖真实 Codex 登录态。

- [ ] **Step 2: 运行 `go test ./adapters/codex -v`，确认新 adapter 测试失败**

- [ ] **Step 3: 实现 `CodexAdapter.Describe`**

检查 command 可执行性并返回 model ID `codex`、能力 metadata、配置中的 runtime model 列表和 session 生命周期模式。

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

覆盖 stream-json、resume、permission、tool event、usage、abort、CLI failure 和 session 生命周期模式。

- [ ] **Step 2: 实现 Claude Code adapter 并运行 `go test ./adapters/claudecode -race`**

- [ ] **Step 3: 为 Kimi 写失败测试并运行**

覆盖 Kimi 原生输出、session resume、usage、abort、异常退出和 session 生命周期模式。

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

Dockerfile 采用多阶段构建：builder 固定 `google/nsjail` 的 tag/commit，安装 autoconf/bison/flex/libprotobuf/libnl 编译依赖，运行镜像复制 nsjail 二进制及其实际依赖；CI 验证 `nsjail --version`、`ldd` 和最小 smoke jail。容器以 supervisor 作为 PID 1，API 和 Worker 进程共享配置目录和受控 workspace root；nsjail 以 user-namespace 无特权运行（不加 CAP_SYS_ADMIN、不 privileged）；SIGTERM 必须按 API -> worker(nsjail) -> CLI 的顺序传播并等待整组回收。

- [ ] **Step 4: 写容器级集成测试**

启动 fake CLI worker，验证 API health、model discovery、stream completion、abort、worker crash 后 API 仍可用、workspace 越界被拒绝（filepath.Rel 校验 + nsjail mount namespace 双重边界）、Agent home/session tmp 隔离和 nsjail preflight fail-closed。

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
