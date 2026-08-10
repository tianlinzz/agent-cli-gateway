# Agent Model Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让 OpenAI `model` 支持 `agent/model` 路由，并让 `/v1/models` 返回与实际调用一致的模型 ID。

**Architecture:** 配置中的 `models` 是部署声明；公开模型 catalog 解析公开 ID 到 adapter ID 和 provider model。Session 保存完整公开 ID，Worker RPC 继续传 adapter ID，并把 provider model 作为可信执行配置传给 adapter。

**Tech Stack:** Go、TOML、protobuf、OpenAI-compatible HTTP API。

---

### Task 1: 配置模型清单

**Files:**
- Modify: `config/gateway.go`
- Test: `config/gateway_test.go`

- [ ] 为 `AgentConfig` 增加 `Models []string`，normalize 去重、裁剪空白，Validate 拒绝空元素和重复模型。
- [ ] 添加配置解析测试，断言 Claude 模型清单保留顺序去重后的稳定结果。
- [ ] 运行 `go test ./config`。

### Task 2: 公共模型 catalog 路由

**Files:**
- Modify: `api/openai/models.go`
- Test: `api/openai/handlers_test.go`

- [ ] 增加公开模型条目结构，生成 `agent/model` 或裸 agent ID。
- [ ] 增加解析函数，把公开 ID 映射为 adapter ID 和 provider model。
- [ ] discovery 只保留 enabled 且 factory/Describe 成功的 adapter；输出排序稳定。
- [ ] 测试 Claude 多模型、Codex 裸 ID、禁用 Agent、缺少 CLI。
- [ ] 运行 `go test ./api/openai`。

### Task 3: Chat 请求使用解析后的模型

**Files:**
- Modify: `api/openai/chat_completions.go`
- Modify: `runtime/contract.go`
- Test: `api/openai/handlers_test.go`

- [ ] 校验公开 model ID 后，将 `StartRequest.ModelID` 设置为 adapter ID，并将 provider model 写入可信的启动配置/metadata。
- [ ] SessionRecord 保存完整公开 model ID，恢复时禁止切换公开模型。
- [ ] 测试 `claude-code/opus` 路由到 Claude adapter 且传递 provider model。

### Task 4: Worker/adapter 传递 provider model

**Files:**
- Modify: `runtime/contract.go`
- Modify: `worker/proto/worker.proto`
- Modify: `worker/rpc.go`
- Modify: `cmd/gateway-worker/main.go`
- Modify: `adapters/claudecode/adapter.go`
- Modify: `adapters/codex/adapter.go`
- Modify: `adapters/kimi/adapter.go`
- Test: `worker/rpc_config_test.go`

- [ ] 在 `AgentExecutionConfig` 中明确 `DefaultModel`/request model 的可信字段，生成 protobuf stub。
- [ ] Worker 根据 adapter ID 构造显式 Options，避免用户 metadata 覆盖部署 command/env。
- [ ] 测试 RPC round-trip 和 Claude/Codex 的 `--model` 参数。

### Task 5: 默认开发配置和文档

**Files:**
- Modify: `config.example.toml`
- Modify: `scripts/dev.sh`
- Modify: `README.md`

- [ ] macOS 开发脚本默认声明 Claude 常用模型清单，Codex 保持裸 ID。
- [ ] 更新 curl 示例和 `/v1/models` 说明。

### Task 6: 全量验证

- [ ] 运行 `go test ./...`、`go vet ./...`、`go build ./...`、`go test -race ./...`。
- [ ] 检查 `git diff --check`，提交一个 scoped commit 并推送 `main`。
