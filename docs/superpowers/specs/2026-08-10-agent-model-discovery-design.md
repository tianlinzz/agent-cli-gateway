# Agent 模型发现与路由设计

## 目标

让 OpenAI 兼容 API 的 `model` 字段同时表达 Agent 类型和 Agent 使用的底层模型，
并保证 `/v1/models` 返回的每个 ID 都可以直接用于 `/v1/chat/completions`。

## 公开模型 ID

模型 ID 采用：

```text
<agent>/<provider-model>
```

示例：

```text
claude-code/sonnet
claude-code/opus
claude-code/haiku
codex
```

当 Agent 配置声明了 `models` 时，只公开带底层模型的 ID；不额外公开裸 Agent
ID。调用 `claude-code/opus` 时，Gateway 内部拆成 adapter `claude-code` 和
CLI model `opus`，最终传给 Claude Code 的 `--model opus`。

当 `models` 为空时，公开裸 Agent ID。该 ID 使用 adapter 自身默认模型，适合
Codex 当前无法由 Gateway 可靠枚举模型的情况。未来 Codex 配置了明确模型清单后，
可以自然扩展为 `codex/<model>`。

## 配置

```toml
[agents.claude-code]
enabled = true
default_model = "sonnet"
models = ["sonnet", "opus", "haiku"]

[agents.codex]
enabled = true
models = []
```

`models` 是部署声明，不由 CLI help 或 provider API 猜测。CLI 是否安装仍由
adapter factory 的实际可执行性检查决定；模型是否获得 provider 授权由调用方和
provider 环境负责。

## 内部路由

Gateway 在收到请求后解析公开 ID：

```text
claude-code/opus
    ├── ModelID: claude-code
    └── Metadata/native execution model: opus
```

Session 绑定完整的公开模型 ID，防止同一个 session 在恢复时切换 Agent 或底层
模型。裸 Agent ID 绑定其默认执行配置。

## Discovery

`/v1/models` 必须使用与 chat invocation 相同的 model catalog：

1. 过滤 `enabled=false` 的 Agent。
2. 解析 adapter factory，CLI 不存在则不公开该 Agent 的模型。
3. 对 `models` 非空的 Agent，生成排序稳定的 `<agent>/<model>` 条目。
4. 对 `models` 为空的 Agent，生成一个裸 Agent 条目。

不会因为 CLI 支持 `--model` 就自动声称所有 provider 模型可用。

## 错误与安全

- 未发现的公开模型返回 OpenAI 兼容的 404 model-not-found。
- 空模型名或非法 `agent/model` 结构返回 400。
- 请求 metadata 不能覆盖 adapter、模型或 command 配置。
- Provider API keys 不进入公开模型 discovery 响应，也不进入用户请求 metadata。

## 验证

- 配置解析和 normalization 测试 `models` 字段。
- discovery 测试 Claude 多模型、Codex 裸 Agent、禁用 Agent、缺少 CLI。
- invocation 测试 `claude-code/opus` 最终把 `opus` 传给 adapter/CLI。
- session 恢复测试不能跨公开模型 ID 复用。
- `go test ./...`、`go vet ./...`、`go build ./...` 通过。
