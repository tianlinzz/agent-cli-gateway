# Agent Gateway 重构设计

## 目标

将项目重写为一个云端可部署的 OpenAI-compatible Agent Gateway。公共入口只提供 OpenAI 风格的模型发现和对话接口；`model` 的值代表一个可执行 Agent，例如 `codex`、`claude-code` 和 `kimi`。Agent 的 CLI 启动、原生协议、session resume、权限事件和进程回收全部封装在独立 adapter/worker 内。

本次重构不承担旧项目兼容，也不迁移 IM 业务。现有仓库和上游 `cc-connect` 只作为 agent CLI 进程管理、原生协议解析和测试经验的来源。

## 范围

第一阶段只接入三个 Agent：

- `codex`
- `claude-code`
- `kimi`

第一阶段公共接口：

- `GET /health/live`
- `GET /health/ready`
- `GET /v1/models`
- `POST /v1/chat/completions`
- `POST /v1/sessions/{id}/abort`

第一阶段不实现旧 `/session`、`/event`、管理 API、provider/MCP HTTP 管理、IM 平台、卡片、cron、timer、relay 和其他 Agent。

## 运行拓扑

第一阶段使用一个容器，但 API 和执行必须是独立进程：

```text
agent-gateway container
  API Gateway (:4096)
        |
        | localhost gRPC or Unix socket
        v
  Worker Supervisor
        |
        +-- Worker: codex + CLI
        +-- Worker: claude-code + CLI
        +-- Worker: kimi + CLI
```

每个 Worker 进程只承载一个 `AgentSession` 和一个 Agent CLI 进程。API 进程不能直接启动 Agent CLI。Worker 崩溃只影响当前 session，不能拖垮 API 进程。

执行通信使用稳定的 Worker RPC contract。第一阶段实现本地进程 backend；未来跨节点时仅替换 backend 和调度器，不修改 OpenAI API 和 Agent Adapter contract。

## 分层职责

### API 层

负责 HTTP 鉴权、请求校验、model 路由、session metadata、workspace ID 解析、SSE/JSON 输出、取消、限流和标准 OpenAI 错误。

API 层不启动 CLI、不解析 Agent 原生协议、不修改 workspace，不包含具体 Agent 分支。

### Runtime 层

负责 model registry、Agent Adapter 路由、session 生命周期、Worker 分配、单 session 单 active turn 约束、超时和事件转发。

核心接口：

```go
type AgentAdapter interface {
    Describe(ctx context.Context) (Descriptor, error)
    Start(ctx context.Context, req StartRequest) (Session, error)
}

type Session interface {
    Send(ctx context.Context, input Input) error
    Events() <-chan Event
    Abort(ctx context.Context) error
    Close(ctx context.Context) error
}

type ExecutionBackend interface {
    Start(ctx context.Context, req StartRequest) (ExecutionHandle, error)
}
```

### Worker 层

负责加载指定 adapter、准备受控 workspace、启动和回收 CLI、设置环境变量、解析原生协议、处理 permission、发送 canonical events、heartbeat 和异常退出。

Worker 对 API 隐藏 Agent 差异，只输出统一的 `AgentEvent`。

### Adapter 层

每个 Agent 独立实现：

- CLI 命令和参数
- native model 配置
- prompt/message 转换
- stream 输出解析
- native session ID resume
- tool call/tool result 映射
- permission request 映射
- usage 读取
- abort 和进程组终止

Adapter 不依赖 HTTP，不依赖 OpenAI JSON，不依赖其他 Agent。

## 模型发现

`GET /v1/models` 返回 Agent catalog。公共 `model` ID 是 Agent ID：

```json
{
  "object": "list",
  "data": [
    {"id": "codex", "object": "model", "owned_by": "agent-cli-gateway"},
    {"id": "claude-code", "object": "model", "owned_by": "agent-cli-gateway"},
    {"id": "kimi", "object": "model", "owned_by": "agent-cli-gateway"}
  ]
}
```

每个 descriptor 还可携带能力 metadata，包括 streaming、tool calls、reasoning、permission、resume 和 multi-turn。Agent 内部实际使用的 LLM 通过配置或结构化扩展参数指定，不改变公共 Agent model ID。

## Chat Completions

请求使用标准 OpenAI Chat Completions 形状，第一阶段支持 `stream=true` 和 `stream=false`。API 层把消息转换为 canonical `AgentInput`，由 adapter 消费；canonical events 再统一转换为 OpenAI response/chunks。

必须处理：

- 客户端取消和连接断开
- session resume
- tool call 事件
- permission request
- usage
- agent 超时、退出和协议错误
- 最终 `[DONE]` 帧

Gateway session ID 通过明确的 gateway header 或 metadata 传递，不能依赖 OpenAI 原生 model 字段承载内部 session 状态。

## 配置

第一阶段只使用 TOML 配置，不提供管理端。配置包含 server、workspace、认证和三个 Agent 的 command、enabled、默认模型、权限模式、超时、并发限制和环境变量。

客户端只能提交 `workspace_id`，由服务端解析到配置允许的 workspace 根目录，不能直接访问宿主机任意绝对路径。

## 状态与安全

第一阶段可以使用内存 session store，但 session record 必须抽象出以下字段：

- gateway session ID
- Agent ID/native session ID
- owner/tenant ID
- workspace ID
- worker ID/node ID
- status、created/updated/expiry time

从第一版开始执行 owner 隔离、单 session 单 active turn、请求超时、进程组回收、日志脱敏和 workspace 边界检查。跨节点的 Redis/数据库持久化、worker registry、scheduler、lease 和故障转移属于后续阶段。

## 目录目标

```text
api/openai/
runtime/
worker/
adapters/claudecode/
adapters/codex/
adapters/kimi/
config/
workspace/
cmd/gateway/
```

旧 `core` 不作为新架构的中心。迁移完成后删除 Platform、MessageHandler、IM Message、旧 session/event API、IM prompt 和管理端等代码。

## 验证标准

在接入每个 Agent 时必须有 adapter regression tests，覆盖启动、输入转换、事件解析、resume、abort、超时和 CLI 异常。Gateway 需要有 OpenAI models、stream/non-stream、cancel、owner isolation 和 worker crash 的集成测试。

第一阶段完成标准是：三个启用的 Agent 能通过同一个 `/v1/chat/completions` 入口完成单轮和多轮流式调用，且 API 进程与 Worker 进程故障互不拖垮。

