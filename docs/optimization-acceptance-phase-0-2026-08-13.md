# Agent Gateway Phase 0 验收报告

- 验收日期：2026-08-13
- 验收人：Codex
- 验收范围：Phase 0（U1、O-A2、O-F03、O-F09a、O-B1、O-B2、O-B3）
- Base commit：`ffaf107e`
- Candidate commit：`7e014f157bcf28a43c4bcb5bfdb537268094602d`
- Branch：`phase-0-correctness`
- 工作树状态：验收时 clean
- 验收依据：`docs/optimization-acceptance-2026-08-13.md`

## 1. 总结论

**结论：不通过。**

候选提交通过了现有全量单测、race、vet、集成测试和两项跨平台构建，但这些测试没有覆盖验收基线要求的关键边界。逐项源码复核确认 U1、O-A2、O-B1、O-B2、O-B3 仍存在实现级缺口，其中 O-A2、O-B2、O-B3 属于当前 Phase 的阻断问题。O-F03 的主信任边界修复成立但证据不完整；O-F09a 通过。

本结论不评价实现投入，只判断 candidate commit 是否满足既定 Phase 0 质量合同。现有 CI 绿色不能抵消已确认的契约缺口。

## 2. 阻断发现

### P0-1：O-A2 在第 257 个积压 notification 后仍会阻塞 JSON-RPC read loop

**源码证据**

- `agent/protocol/jsonrpc.go:81-85` 明确说明永久阻塞的 consumer 会填满 256 队列，随后 read loop 阻塞。
- `agent/protocol/jsonrpc.go:246-251` 仍由 read loop 对 `notifyQueue` 执行阻塞 send；队列满后无法继续读取后续 RPC response。
- `agent/protocol/jsonrpc_test.go:103-151` 的回归测试只阻塞一个 notification，未填满队列，因此只能证明短时 burst 解耦，不能证明持续背压下控制流存活。

**影响**

当下游 event consumer 永久停滞且 native CLI 继续产生超过 256 条 notification 时，response correlation 再次停止。Abort/cancel 所需的响应也可能无法读取，原始活性问题只是推迟发生，并未闭环。

**复验条件**

1. 明确事件优先级和 overflow 策略。
2. read loop 不得因展示事件队列满而永久阻塞。
3. 测试必须主动阻塞 consumer、填满并超过队列，再证明独立 RPC response 和 reverse request 仍可处理。
4. `finish`、`error`、permission/reverse request、native identity 等不可丢事件必须有确定性测试。
5. 覆盖 overflow、Close、Abort、notification flood 的 race/goroutine 稳态。

### P1-2：O-B2 仅将 gRPC 限制提高到 16 MiB，没有 canonical event size contract

**源码证据**

- `worker/rpc.go:87-90` 将默认 4 MiB 直接改为固定 16 MiB。
- `worker/rpc.go:486-518` 的 `toFrame` 原样复制 `Text`、`Error`、tool、permission detail 和 reasoning text，没有统一大小检查、截断标记或 typed overflow error。
- `worker/rpc_event_test.go` 只覆盖 reasoning round-trip，没有上限内、边界、超限和极端超限测试。

**影响**

大于 16 MiB 的单事件仍会由 gRPC 随机中断 stream/session；并发大事件仍扩大内存压力。当前改动只移动失败阈值，没有定义受控超限行为。

**复验条件**

1. 在进入 RPC 前定义 canonical event/字段上限。
2. tool output、text、reasoning、error 等超限时返回带 marker 的截断事件或 typed error。
3. gRPC limit 只作为略高于 canonical 上限的防御边界，client/server 一致。
4. 增加边界值、极端值、真实 stream 和并发内存测试。

### P1-3：O-B3 的 `client`/`workerPID` 同步仍不完整

**源码证据**

- `worker/supervisor.go:952-964` 对 handshake 写入加锁，但 `worker/supervisor.go:972-976,989` 又无锁写 `ws.client = nil`。
- `worker/supervisor.go:617,652,685-687,820` 多处直接无锁读 `ws.client`，没有使用新增的 `snapshotClient()`。
- `worker/supervisor.go:407`、`worker/spawn_unix.go:52-54`、`worker/spawn_windows.go:50` 直接无锁读 `workerPID`。
- 新增的 `snapshotClient()` 仅被 heartbeat 使用，不能形成统一 publication contract。
- 没有使用 barrier 强制 handshake/terminate/killGroup/Send/Abort 交错的确定性回归测试。

**影响**

race detector 本次未触发不代表同步正确。源码仍保留未同步读写路径，且 teardown、handshake failure 和直接信号路径正是原问题的并发窗口。

**复验条件**

1. 建立单一 publication/snapshot 规则，所有并发访问遵守同一锁或原子契约。
2. 不在持锁状态执行 RPC、Close、Wait 或 channel drain。
3. 使用 barrier/fake client 强制 handshake、terminate、heartbeat、killGroup、Send/Abort 交错。
4. 专项测试在 `-race` 下稳定通过多轮。

### P1-4：U1 会回收仍在运行的长 turn，且 dead handle 终态没有即时闭环

**源码证据**

- `api/openai/chat_completions.go:296-303` 只在 turn 开始时 Touch TTL；turn 运行期间不会续期。
- `runtime/session_store.go:241-250` 的 `Prune` 只检查 `ExpiresAt`，不排除 `SessionTurnActive`。
- `api/openai/types.go:342-349` prune 后立即删除并关闭 handle。
- 因此只要 turn 时长超过 record TTL，后台 prune 就可能删除 active record 并关闭正在执行的 handle。
- `api/openai/types.go:331-350` 仅按 TTL 清理 handle；没有监听 handle `Done()` 后即时从 map 删除的终态闭环。
- `dropHandleByID` 只处理 `handles`，没有同步清理 `serverToolIDs` 等 session 附属状态。
- 测试只创建 50 条 store record 和一个 API handle；没有验收要求的 1000 session churn、active-turn prune、dead-handle Done、附属 map 或 goroutine 稳态证据。

**影响**

这既存在长 turn 被错误终止的正确性风险，也没有完成原 U1 的 Worker 终态回收闭环。默认 TTL 为 168h 只降低触发频率，不能证明状态机正确。

**复验条件**

1. active turn 在任何 TTL/prune 交错下都不能被删除或关闭。
2. 明确哪些活动刷新 TTL，使用可控时钟覆盖 begin/end/失败/边界。
3. Worker `Done()`、idle reclaim、heartbeat failure、自然退出、显式 Close、启动失败都能释放 cached handle。
4. session record 删除同步清理其附属 ledger/state。
5. 增加至少 1000 session churn，证明 store、handle、ledger、goroutine 和 Worker 数回落。

### P1-5：O-B1 没有满足“明确上限、截断 marker、UTF-8 安全”契约

**源码证据**

- `agent/process/process.go:192-215` 允许 buffer 常态增长到约 `2*max` 后才裁剪，最终长度并不严格限制为配置的 `max`。
- 单次超大 `Write` 会先完整 append，再裁剪，瞬时分配仍可远超上限。
- `agent/process/process.go:219-222` 直接将任意尾部字节转为 string，可能从 UTF-8 多字节字符中间截断。
- 输出没有 truncation marker，调用方无法区分完整 stderr 与被截断内容。
- `agent/process/process_test.go:18-40` 将 `<= 2*max` 当作通过，只覆盖 ASCII 单线程写入，没有 marker、UTF-8、单次超大 write 或并发测试。

**影响**

内存已从完全无界改善为周期性裁剪，但尚未满足验收基线定义的 bounded tail 行为和可诊断性。

**复验条件**

1. 写入完成后的保留内容严格受明确字节上限控制。
2. 单次极大 write 不保留不必要的完整输入副本。
3. 截断结果有 marker，并保证返回字符串 UTF-8 有效。
4. 覆盖并发 Write/String、单次极大输入、UTF-8 边界和 native failure 诊断。

## 3. 逐项验收结论

| ID | 结论 | 说明 |
|---|---|---|
| U1 | 不通过 | TTL/prune 基础链路已接通，但 active-turn 安全、Done 终态、附属状态和千级稳态未闭环。 |
| O-A2 | 不通过 | 只能吸收 256 条 burst；队列填满后 read loop 仍阻塞，overflow/不可丢事件契约缺失。 |
| O-F03 | 有条件通过 | API 已按大小写无关规则剥离四类 resume key，并由 SessionRecord 注入 canonical ID；缺三 Agent table test、跨 caller/model/workspace 组合和 Worker crash 后 server-owned resume 专项证据。 |
| O-F09a | 通过 | enabled Codex/Claude 配 timeout 会 fail closed，Kimi 被明确允许，示例配置同步更新。 |
| O-B1 | 不通过 | 已有有界化方向，但严格 cap、marker、UTF-8 与并发证据缺失。 |
| O-B2 | 不通过 | 仅提高 gRPC limit，不满足 canonical event size contract。 |
| O-B3 | 不通过 | 加锁覆盖不完整，仍存在多处未同步访问且没有确定性交错测试。 |

## 4. O-F03 残余证据要求

O-F03 的核心修复路径成立：

- `api/openai/chat_completions.go:120-145` 在外部 metadata 进入 runtime 前剥离 resume keys。
- `api/openai/chat_completions.go:327-332` 只从 server-owned SessionRecord 注入 `native_session_id`。
- 现有 model/workspace/owner 检查保持。

但正式“通过”还需要补齐：

1. Codex、Claude Code、Kimi table-driven 测试，而不是只检查一个 Codex HTTP 请求到 fake backend。
2. 新 session 和已有 session 都注入冲突 key。
3. 跨 caller、跨 model、跨 workspace 负向组合。
4. Worker crash 后使用保存的 server-owned native ID 恢复，且攻击者值未进入 adapter。
5. 检查日志和错误响应不回显 native ID。

## 5. 架构不变量检查

| 不变量 | 结果 | 证据 |
|---|---|---|
| Native tools 不映射为 OpenAI tool calls | 通过 | 相关 arch/API 测试通过，本阶段 diff 未改变映射契约。 |
| API 不直接启动 CLI | 通过 | `internal/archtest` 通过。 |
| runtime/API name-agnostic | 通过 | `internal/archtest` 通过；runtime 未新增 agent 分支。 |
| prod/dev isolation fail-closed | 通过（本阶段未改） | 配置校验与集成 preflight 测试通过。 |
| unknown outcome 不自动重放 | 通过（本阶段未发现回归） | 现有集成和代码审查未见新增自动重试路径。 |
| owner/session/workspace 隔离 | 通过（O-F03 尚缺专项组合证据） | 现有 owner/model/workspace 检查与测试通过。 |

## 6. 命令结果

以下命令均在 candidate commit 上于 2026-08-13 新鲜执行：

| 命令 | 结果 | 摘要 |
|---|---|---|
| `go test ./... -count=1` | 通过 | 全部包退出码 0，integration 约 50.7s。 |
| `go test -race ./... -count=1` | 通过 | 全部包退出码 0，未检测到 race。该结果不消除源码中未同步访问。 |
| `go vet ./...` | 通过 | 退出码 0。 |
| `go test ./integration/ -v -count=1` | 通过 | 13 个集成场景全部通过，约 37.3s。 |
| `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...` | 通过 | 退出码 0。 |
| `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...` | 通过 | 退出码 0。 |
| `go test ./internal/archtest/ -v -count=1` | 通过 | 5 项架构守护测试通过。 |
| O-A2 现有专项测试 `-count=20` | 通过 | 只覆盖 1 条 stalled notification，未覆盖 queue overflow。 |
| U1/O-F03 现有专项测试 `-count=10` | 通过 | 未覆盖验收缺口。 |
| O-B1 现有专项测试 `-count=20` | 通过 | 测试标准仅要求 `<= 2*max`。 |
| O-B3 相关 worker race 测试 `-count=10` | 通过 | 没有强制原始竞态窗口。 |
| `git diff --check ffaf107e..HEAD` | 不通过 | 验收文档头部 3 行使用 Markdown hard-break 尾随空格；非代码功能阻断，但最终提交需清理。 |

## 7. Red-Green 证据状态

提交说明声称 O-A2 回归测试已在 pre-fix 代码失败，但本次交付没有附原始 red-green 日志。本次验收只确认 candidate 上为 green，没有破坏工作树去回退修复，因此以下各项仍需实施方提供或通过隔离 worktree 重演：

- U1 session/handle leak
- O-A2 stalled/overflow notification
- O-F03 client resume ID injection
- O-F09a silent timeout configuration
- O-B1 stderr unbounded growth
- O-B2 oversized event stream
- O-B3 handshake/teardown race

其中 O-A2、O-B2、O-B3 当前连 candidate-side 完整验收测试都未达到，补 red-green 日志本身不能使其通过。

## 8. 压力与故障矩阵

| 场景 | 结论 | 原因 |
|---|---|---|
| 千级 session churn | 无法验收 | 没有 1000 session 及 store/handle/ledger/goroutine 稳态证据。 |
| 事件消费者阻塞并超过队列 | 不通过 | 代码明确在队列满后阻塞 read loop。 |
| 超大 tool output | 不通过 | 没有 canonical 上限；超过 16 MiB 仍由 gRPC 断流。 |
| Abort/timeout | 现有集成通过 | 未发现本阶段新增自动重放；O-A2 overflow 下 abort response 仍有活性风险。 |
| Worker/CLI crash | 现有集成部分通过 | API 可继续使用；server-owned native resume 专项证据缺失。 |
| Gateway shutdown | 现有测试通过 | 不足以覆盖所有新增 prune/drain goroutine 交错。 |

## 9. 复验最小范围

Phase 0 复验不要求推翻现有拆分，可在当前分支追加窄修复，但至少需要：

1. 修复并补测 O-A2 overflow 与不可丢事件契约。
2. 为 O-B2 引入 canonical event size contract，而不是继续增大 gRPC limit。
3. 统一 O-B3 的 `client`/`workerPID` publication 和所有访问路径。
4. 修复 U1 active-turn prune，并闭合 Worker Done/附属状态回收与千级稳态证据。
5. 完成 O-B1 strict cap、marker、UTF-8 和并发行为。
6. 补齐 O-F03 三 Agent 与 crash-resume 负向/正向证据。
7. 提供所有 bug fix 的可复现 red-green 记录。
8. 重跑本报告第 6 节全部命令并修复 `git diff --check`。

复验应提交新的不可变 candidate commit；不得基于 dirty worktree 签发通过结论。
