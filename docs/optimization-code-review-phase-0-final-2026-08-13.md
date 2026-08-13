# Agent Gateway Phase 0 最终 Code Review

- Review 日期：2026-08-13
- Previous candidate：`7e014f157bcf28a43c4bcb5bfdb537268094602d`
- Current candidate：`e738ea54`
- Branch：`phase-0-correctness`
- Review 基线：`ffaf107e`
- 工作树：candidate 代码提交无未提交改动；验收文档为未跟踪文件，不属于 candidate

## 结论

**不通过，暂不允许 Phase 0 合并或进入下一阶段。**

U1、O-B1、O-B2、O-B3、O-F03、O-F09a 的上一轮阻断已完成对应修复和专项测试。但 O-A2 仍有 P0 缺陷：队列满时虽然 reader 不再阻塞，却会对所有 notification 统一丢弃，可能丢失 Codex/Kimi 的 turn 终态、native session identity 或 usage。当前测试没有证明 overflow 后自治 turn 仍能可靠结束。

## P0-1：O-A2 丢失控制 notification

证据：

- [enqueueNotify](/Users/tl/workspace/agent-cli-gateway/agent/protocol/jsonrpc.go:122) 队列满时对所有 notification 统一 `drop + count`。
- [readLoop](/Users/tl/workspace/agent-cli-gateway/agent/protocol/jsonrpc.go:293) 没有区分控制事件和展示事件。
- [overflow handler](/Users/tl/workspace/agent-cli-gateway/agent/codex/session.go:434) 只发 `EventReasoning` 截断标记，不能恢复 native state 或 `EventFinish`。
- Codex 的 `turn/completed`、thread 更新和 usage 都依赖 notification：[session.go](/Users/tl/workspace/agent-cli-gateway/agent/codex/session.go:193)、[session.go](/Users/tl/workspace/agent-cli-gateway/agent/codex/session.go:341)、[session.go](/Users/tl/workspace/agent-cli-gateway/agent/codex/session.go:420)。
- [Sync](/Users/tl/workspace/agent-cli-gateway/agent/protocol/jsonrpc.go:191) 在队列饱和时直接返回，不会恢复已丢事件。
- [TestJSONRPCOverflowNeverBlocksReader](/Users/tl/workspace/agent-cli-gateway/agent/protocol/jsonrpc_test.go:154) 只证明 response/reverse request 继续处理，没有证明 finish、native identity、usage 在 overflow 下可靠。

影响：`turn/completed` 被丢后 API 可能等待 Gateway timeout；thread 更新被丢后 server-owned native ID 可能无法保存；usage 可能丢失或顺序错误。Kimi 的 `session/update` 终态同样未证明。这不是 telemetry 丢失，而是会改变 Gateway 状态机和恢复语义，按 P0 处理。

必须采用以下任一等价契约：

1. 将 finish、error、permission/reverse request、native session identity、usage/turn completion 放入不可丢独立路径，展示 text/reasoning/tool progress 才允许丢弃或聚合。
2. 或在丢弃展示 notification 后，通过 native 状态查询/response 或明确 teardown 生成确定终态，不能把 unknown outcome 当成普通 stop。

必须补充 Codex/Kimi overflow 下 finish/error/native identity/usage 测试，以及 API stream/non-stream 终态一致性测试。

## 已复核通过项

| ID | 结论 | 证据 |
|---|---|---|
| U1 | 通过 | `runtime/session_store.go:287` active-turn prune 豁免；`api/openai/types.go:463` Done watcher；`handlers_test.go:373` 1000 session reclaim。 |
| O-B1 | 通过 | `agent/process/process.go:230` strict tail；`:245` marker；UTF-8、超大写入、并发测试已补齐。 |
| O-B2 | 通过 | `worker/rpc.go:98` canonical 1 MiB field limit；tool/text/error/reasoning/UTF-8/极端值测试已补齐。 |
| O-B3 | 通过 | `Send`/`Abort`/`Close`/`bridge`/heartbeat/terminate/killGroup 使用 snapshot/clear；`supervisor_test.go:193` lifecycle race test。 |
| O-F03 | 通过 | 三 Agent 新/已有 session、攻击者 key 剥离、跨 caller 404 测试已补齐。 |
| O-F09a | 通过 | Codex/Claude timeout fail closed，Kimi 明确允许。 |
| O-A2 | 不通过 | reader 活性修复成立，但控制 notification 可靠性未闭环。 |

## 验证命令

以下命令在 current candidate 上新鲜执行，均退出码 0：

- `go test ./... -count=1`
- `go test -race ./... -count=1`
- `go vet ./...`
- `go test ./integration/ -v -count=1`
- `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`

本机为 Darwin；没有以本机结果替代真实 Linux nsjail、PID namespace、seccomp 或 arm64 runtime smoke。Phase 0 未修改这些隔离边界，仍按 Phase 1 门禁验收。

## 非阻断观察

- `worker/spawn_unix.go:46` 在 test/direct mode 可能对不属于当前进程组的 PID 发 signal，出现 `operation not permitted` warning；integration 仍通过，暂不改变本 Phase 结论。
- 最终 candidate 的 tracked diff 仍需通过 `git diff --check`。

## 复验条件

1. O-A2 区分控制 notification 与展示 notification 的可靠性等级。
2. overflow 下 Codex/Kimi 的 finish、error、native identity、usage 有确定性测试。
3. API stream/non-stream overflow 终态一致，unknown outcome 不伪装为普通 stop。
4. 提交新的 clean candidate commit，并重跑上述全部命令。
5. U1、O-B1、O-B2、O-B3、O-F03、O-F09a 维持当前通过状态。
