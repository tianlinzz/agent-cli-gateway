# Agent Gateway 代码审计与优化方案

> 审计日期：2026-08-13
> 审计范围：`runtime/` · `api/openai/` · `worker/`（supervisor / rpc / nsjail）· `agent/`（process / protocol / codex / claudecode / kimi）· `adapters/`
> 代码规模：约 20,100 行 Go（含测试）
> 审计基线：commit `ffaf107e`（main）
> 文档用途：**供与其他优化方案交叉对比后决定取舍**。本文档不主张立即执行，仅提供证据与分级。

---

## 1. 执行摘要

`agent-cli-gateway` 是一个 OpenAI 兼容的 Agent 网关，把 Codex / Claude Code / Kimi 统一暴露成 `/v1/*`，核心卖点是 **进程隔离优先的执行模型**：每个 session fork 一个 nsjail 包裹的 worker 子进程，API 进程从不直接启动 Agent CLI。

**总体结论**：架构骨架扎实、工程纪律到位（分层由 `internal/archtest` 强制；测试覆盖真实子进程/信号/gRPC/崩溃恢复/竞态）。当前主要债务集中在三方面：

1. **长跑稳定性**——session 记录与 handle 无界增长（内存泄漏）、JSON-RPC 读循环被满 event channel 阻塞（活性风险）。
2. **隔离与进程治理**——共享 PID namespace + seccomp 放行 `kill` 导致跨会话信号风险；硬杀路径遗漏 CLI 进程组。
3. **三层 Agent 代码重复**——event 类型、adapter 桥、codex/kimi JSON-RPC 生命周期大量 copy-paste 且已**行为漂移**。

判断：项目正处于 **"从能跑通到能长跑"的拐点**。若计划长跑或多人协作，第一、二类应优先于重构。

---

## 2. 方法论

- 五个层次并行深度阅读（runtime 契约、HTTP/SSE 层、worker 隔离层、agent 原生协议层、adapter 桥层）。
- **所有发现均逐条对照实际源码验证**，附 `file:line` 证据；1 项初步怀疑经核验后判定为误报（见 §5）。
- 验证手段：代码精读 + `grep` 调用点确认（如 `Prune`/`Touch`/`Delete` 的真实调用方）+ 配置默认值与挂载语义核对。
- 分级标准：
  - **严重度** — P0（正确性/安全/泄漏，影响长跑）· P1（鲁棒性/竞态）· P2（加固/一致性）· P3（可维护性/死代码）
  - **工作量** — S（<半天）· M（1-2 天）· L（>2 天，需跨文件或需真机验证）
  - **风险** — 改动引入回归的概率与爆炸半径

---

## 3. 发现总览矩阵

| ID | 发现 | 类别 | 严重度 | 工作量 | 风险 | 依赖 |
|----|------|------|--------|--------|------|------|
| A1 | Session 记录 & Handle 无界增长（内存泄漏） | 稳定性 | **P0** | M | 低 | 无 |
| A2 | JSON-RPC 读循环被满 event channel 阻塞 | 稳定性 | **P0** | M | 中 | 无 |
| A3 | 启用 PID namespace（治硬杀遗漏 CLI + 跨会话 kill） | 稳定性+隔离 | **P0** | L | 中高 | 需 Linux-CI 验证 |
| B1 | agent/process stderr 无界缓冲 | 鲁棒性 | P1 | S | 低 | 无 |
| B2 | gRPC 默认 4 MiB 消息上限 | 鲁棒性 | P1 | S | 低 | 无 |
| B3 | `ws.client`/`ws.workerPID` 无锁读写（data race） | 鲁棒性 | P1 | S | 低 | 无 |
| B4 | codex Abort 在未拿到 turn id 时硬失败 | 鲁棒性 | P1 | M | 中 | 无 |
| B5 | `terminate` 在进程组退出前 RemoveAll 目录 | 鲁棒性 | P1 | S | 低 | 与 A3 相关 |
| B6 | 热循环 `time.After` 未停止（timer 泄漏） | 鲁棒性 | P2 | S | 低 | 无 |
| C2 | seccomp-off 仅在 config 层校验，缺分层校验 | 隔离 | P2 | S | 低 | 无 |
| C3 | uidmap 默认值（1000 vs 65532）部署 foot-gun | 隔离 | P2 | S | 低 | 无 |
| C4 | `/etc` 整目录只读挂载暴露面偏大 | 隔离 | P3 | S | 低 | 无 |
| D1 | 三层 agent event 类型重复 | 可维护性 | P3 | M | 低 | 无 |
| D2 | codex/kimi JSON-RPC 生命周期重复且行为漂移 | 可维护性 | P3 | L | 中 | 可借 D1 |
| D3 | adapter 共享 helper 三份重复 | 可维护性 | P3 | M | 低 | 无 |
| D4 | 死代码/死配置 + 超时返回不一致 | 可维护性 | P3 | S | 低 | 无 |

> 另有 **1 项误报**（`stream.go` deferredAbort）见 §5。

**关键洞察**：A3（PID namespace）是**一招解两题**——同时关闭硬杀遗漏 CLI（A3-①）与跨会话 kill 向量（原属隔离类）。建议作为隔离类问题的主修复手段，C2/C3/C4 为补充。

---

## 4. 逐项详情

### 🔴 Tranche A — 长跑稳定性

---

#### A1. Session 记录与 Handle 无界增长（内存泄漏）

| 项 | 内容 |
|----|------|
| 严重度 | **P0** |
| 根因 | `SessionsConfig`（`config/gateway.go:79-86`）只控制 supervisor 层 idle **worker 进程**回收（`supervisor.go:456 reapIdleLoop`，2h 后 reap 进程）。但 API 层 `SessionStore.byID` 记录**从不设置 `ExpiresAt`**——`expired()`（`session_store.go:274`）要求 `!ExpiresAt.IsZero()`，默认零值恒为 false。`Prune`/`Touch` 是**死代码**（全仓非测试调用点：`store.Delete` 仅 `chat_completions.go:459` abort 全删路径一处）。`Handler.handles`（`types.go:99`）同理：idle worker 被 reap 后若无后续请求，`dropHandle`（`types.go:416`）永不触发。 |
| 代码证据 | `runtime/session_store.go:213 Touch`、`:238 Prune`（无调用方）；`api/openai/types.go:99 handles`；`config/gateway.go:239-241`（仅 IdleTimeout/ReapInterval，无记录 TTL） |
| 影响 | 长跑下 `byID`、`handles` 两个 map 每个 session 加一条且永不回收 → 内存单调增长直至 OOM。 |
| 修复方向 | ① `config` 新增 `SessionsConfig.RecordTTL`（默认 24h，**记录寿命 > 进程寿命**以保留 resume 能力）；② `Handler` 起后台 `pruneLoop`（按 `ReapInterval` 周期 `store.Prune(now)`，记录被 prune 时连带 `dropHandle`）；③ 在 `BeginTurn`/`EndTurn`/收到 `EventNativeSession` 等活动点调用 `store.Touch(id, callerID, RecordTTL)`。supervisor 的 idle reap 不删记录（保留 `NativeSessionID` 供 resume）。 |
| 工作量 | M |
| 风险 | 低。Touch/Prune/TTL 契约均已存在，只是未接线。 |
| 回归测试 | `session_store_test.go` 加记录到期被 Prune；`integration` 加 idle-record 回归（快进时钟 → Prune 删除）。 |

---

#### A2. JSON-RPC 读循环被满 event channel 阻塞（活性风险）

| 项 | 内容 |
|----|------|
| 严重度 | **P0** |
| 根因 | `agent/protocol/jsonrpc.go:160-166` 在 `readLoop` 内 **inline** 调用 `c.notify(msg)`。codex/kimi 的 notify 最终 `emit` 到 64 缓冲的 events channel（`agent/codex/session.go:427-432`、`agent/kimi/session.go:345-350`）。消费方一旦停止排空（或 burst >64），readLoop 阻塞在 `emit`，**所有 RPC 响应（含在飞 turn 的 `turn/start`、`session/prompt` 响应）全部卡死**，turn 可能无限挂起。 |
| 代码证据 | `agent/protocol/jsonrpc.go:152-182 readLoop`、`:160-166 inline notify` |
| 影响 | 消费方背压传导到协议读循环，整条 JSON-RPC 连接失活。是当前最高严重度的潜在 liveness 故障。 |
| 修复方向 | `jsonrpc.go` 引入内部有界队列（如 256）+ 单个 drain goroutine 串行调用 `notify`；readLoop 只负责解码+入队，**永不阻塞在消费方**。保留通知相对顺序（单 worker 消费）。`terminate` 一并 settle drain goroutine。 |
| 工作量 | M |
| 风险 | 中。需保证通知顺序与 `terminate` 时 drain goroutine 的正确收敛。 |
| 回归测试 | `jsonrpc_test.go` 加 `TestNotifyDoesNotBlockReadLoop`——填满下游 channel 后验证 RPC 响应仍能返回。 |

---

#### A3. 启用每会话 PID namespace（治硬杀遗漏 CLI + 跨会话 kill）

| 项 | 内容 |
|----|------|
| 严重度 | **P0**（一招解两题） |
| 根因① | `agent/process/process_unix.go:16` 给 CLI `Setpgid:true` → CLI 独立进程组，**逃出** supervisor 的 `killGroup(-nsjail_pgid)`（`worker/spawn_unix.go:46`）。硬杀路径（崩溃 reap `supervisor.go:760` / SIGKILL 升级 `:690` / 心跳 kill `:869`）只能靠 **stdin EOF** 间接终止 CLI，不可靠（卡死或不监听 stdin 的 CLI 会残留）。`spawn_unix.go:17-19` 注释声称 "signal -pgid 覆盖整棵树"，**实际未覆盖 CLI 组**。 |
| 根因② | `profile.go:103` `clone_newpid:false`（共享 PID namespace）+ seccomp 放行 `kill/tkill/tgkill`（`profile.go:272`）。被攻陷的 Agent 可信号**同 UID 的网关及所有兄弟 worker**。文档 k8s 部署 runAsUser 65532，jail 外 UID == 网关 UID。 |
| 代码证据 | `worker/nsjail/profile.go:103,272`；`agent/process/process_unix.go:16`；`worker/spawn_unix.go:43-57` |
| 修复方向 | `profile.go` 改 `clone_newpid: true`（可配置，默认 true）。nsjail 成为 jail 内 PID 1；supervisor 杀掉它时**内核向该 namespace 内所有进程发 SIGKILL** → 整棵树（含 CLI 独立组）可靠终止；同时 CLI 无法看到/信号宿主或兄弟会话 → 跨会话 kill 向量消失（seccomp 可继续保留 `kill` 供 Agent 自身进程管理，因 PID ns 隔离后 kill 只能命中 namespace 内 PID）。配套：jail 内挂只读 `/proc`（部分 CLI 读 `/proc/self`）。 |
| 工作量 | L |
| 风险 | 中高。需真机 CLI 验证 `/proc` 可见性与 PID-1 行为；darwin 开发机无法验证 PID ns。**default-flip 以 Linux-CI smoke 通过为准**，保留 `clone_newpid` 开关便于回退。 |
| 依赖 | `docker/nsjail-smoke.sh` 加 "fork 子进程后杀 nsjail → 子进程随之死亡" 断言（Linux-CI only）。 |
| 回归测试 | nsjail-smoke 新断言；`profile_test.go` 更新断言。 |

---

### 🟡 Tranche B — 鲁棒性修补

---

#### B1. agent/process stderr 无界缓冲

| 项 | 内容 |
|----|------|
| 严重度 | P1 |
| 根因 | `agent/process/process.go:188-203 lockedBuffer` 无上限累积全部子进程 stderr，仅在 teardown 时 `StderrString()` 读取。claudecode 以 `--verbose` 启动（`agent/claudecode/launch.go:19`），长 session 可耗尽 worker 内存。 |
| 修复方向 | 改为有界 ring buffer（如 256 KiB），保近期尾部用于排障。 |
| 工作量 | S · 风险 低 · 测试：填充超限 stderr 验证封顶且保留尾部。 |

---

#### B2. gRPC 默认 4 MiB 消息上限

| 项 | 内容 |
|----|------|
| 严重度 | P1 |
| 根因 | `worker/rpc.go:192 grpc.NewServer()` 无选项，默认 4 MiB recv/send。入站 `SendInput` 已被 HTTP body 1 MiB 限制（`chat_completions.go:18`）保护，但**出站 EventFrame 无界**——单个 >4 MiB 的 tool result 会断流并杀死会话。 |
| 修复方向 | server/client 设 `MaxSendMsgSize/MaxRecvMsgSize`（如 16 MiB）；源端可另加 tool-result 截断。 |
| 工作量 | S · 风险 低 · 测试：构造超长 tool result 验证不断流。 |

---

#### B3. `ws.client` / `ws.workerPID` 无锁读写（data race）

| 项 | 内容 |
|----|------|
| 严重度 | P1 |
| 根因 | `handshake` 写 `ws.client`（`supervisor.go:930`）、`ws.workerPID`（`:938`）未持 `ws.mu`；`terminate`（`:873-874`）、`killGroup`（`spawn_unix.go:52`）、heartbeat（`:779`）读这些字段也未持锁。monitor 在 handshake 前已启动（`:355`），故读写可并发。窗口窄（`-race -count=3` 未复现），但是真实 Go race。 |
| 修复方向 | 用 `ws.mu` 保护这两个字段。 |
| 工作量 | S · 风险 低 · 测试：`go test -race ./worker/`。 |

---

#### B4. codex Abort 在未拿到 turn id 时硬失败

| 项 | 内容 |
|----|------|
| 严重度 | P1 |
| 根因 | `agent/codex/session.go:486-488`：`Abort` 在 `s.active.id == ""` 时返回 `"abort: active turn has no native id"` 且**不升级**，CLI 继续跑该 turn。恢复仅靠 API 层 31s `abortAndDrain` settle 后 `Close`→SIGKILL（`api/openai/types.go:512-528`），一次与 fresh `Send` 竞态的 abort 退化成整 session 拆除。 |
| 修复方向 | 未拿到 id 时标记 aborted 由完成路径观测，或降级到进程拆除作为兜底。 |
| 工作量 | M · 风险 中 · 测试：abort 与 fresh Send 竞态回归。 |

---

#### B5. `terminate` 在进程组退出前 RemoveAll 目录

| 项 | 内容 |
|----|------|
| 严重度 | P1 |
| 根因 | `supervisor.go:880-893` 在 `killGroup(SIGKILL)` 后立即 `os.RemoveAll(sessionDir/agentHomeDir/socketDir)`，不等进程组真正退出。仍在写的 CLI（见 A3-①）可能与 unlink 竞态导致部分状态。 |
| 修复方向 | RemoveAll 前 bounded-wait 进程组退出（`reaped` 已覆盖直接子，需覆盖整组）。**与 A3 相关**——A3 落地后整组随 PID ns 死亡，此问题同步消解。 |
| 工作量 | S · 风险 低。 |

---

#### B6. 热循环 `time.After` 未停止（timer 泄漏）

| 项 | 内容 |
|----|------|
| 严重度 | P2 |
| 根因 | `aggregateTurn`（`chat_completions.go:532,585`）、`streamTurn`（`stream.go:193,297`）用 `time.After` 后从不 Stop；提前完成的 turn 留下 10 分钟 `turnTimeout` timer + 250ms grace timer 直到到期。高 turn 率下累积垃圾。 |
| 修复方向 | 改 `time.NewTimer` + `defer Stop()`。 |
| 工作量 | S · 风险 低。 |

---

### 🟢 Tranche C — 隔离与安全加固

> **C1（跨会话 kill）已并入 A3**——PID namespace 是其主修复手段。下列为补充加固。

---

#### C2. seccomp-off 仅在 config 层校验，缺分层校验

| 项 | 内容 |
|----|------|
| 严重度 | P2 |
| 根因 | supervisor 层 `Config.validate()`（`supervisor.go:104-124`）校验 mode/isolation/workspace/binary，但**不校验 `Seccomp.Policy`**；`nsjail.Build` 对任意 mode 接受 `SeccompOff`（`profile.go:144-146`）。限制只活在 `config.GatewayConfig.Validate()`（`config/gateway.go:380-387`）。直接构造 Supervisor（测试或未来调用方）用 prod+seccomp-off 会静默跑无过滤。 |
| 修复方向 | supervisor 层与 nsjail.Build 增加同样的 seccomp 约束（fail-closed）。 |
| 工作量 | S · 风险 低。 |

---

#### C3. uidmap 默认值（1000 vs 65532）部署 foot-gun

| 项 | 内容 |
|----|------|
| 严重度 | P2 |
| 根因 | Go `DefaultGatewayConfig()` uidmap 1000:1000（`config/gateway.go:262-263`）与容器 entrypoint 65532:65532（`docker/entrypoint.sh`）不一致。非特权进程无法把 outside_id=1000 映射到非自身 UID；以容器 user 65532 跑 Go 默认配置会使 nsjail 建 uid map 失败 → preflight/start fail-closed（安全但部署踩坑）。 |
| 修复方向 | 对齐默认值，或按当前运行 UID 派生 uidmap。 |
| 工作量 | S · 风险 低。 |

---

#### C4. `/etc` 整目录只读挂载暴露面偏大

| 项 | 内容 |
|----|------|
| 严重度 | P3 |
| 根因 | `profile.go:131-135` 整个 `/etc` 只读挂载进 jail，暴露宿主 world-readable 文件。单租户容器内影响低，但"无网关运行时/配置被挂载"的边界声明比实际挂载集更宽。 |
| 修复方向 | 收窄到具体文件（`resolv.conf` / ssl certs）。 |
| 工作量 | S · 风险 低。 |

---

### 🔵 Tranche D — 架构去重与死代码清理

---

#### D1. 三层 agent event 类型重复

| 项 | 内容 |
|----|------|
| 严重度 | P3 |
| 根因 | `agent/{codex,claudecode,kimi}` 的 EventKind 常量、`Event`/`Usage`/`ToolCall`/`PermissionRequest`/`Reasoning`/`Input` 结构近乎逐字节重复（~50 行 ×3），且都镜像 `runtime.Event`。 |
| 修复方向 | 提到共享 `agent/events` 包（或在合适处复用 `runtime.Event`）。 |
| 工作量 | M · 风险 低 · 是 D2 的前置。 |

---

#### D2. codex/kimi JSON-RPC 生命周期重复且行为漂移

| 项 | 内容 |
|----|------|
| 严重度 | P3 |
| 根因 | codex 与 kimi 的 `Start`+`initialize`+`monitor`+`cleanupFailedStart`+`abortEscalation`+`Close` 结构同构（~200 行），仅方法名（`thread/start` vs `session/new`）、turn 模型（通知驱动完成 vs 阻塞 RPC）、通知派发不同。**复制后已行为漂移**：Close 语义（codex/kimi 实际总是 SIGKILL，`codex/session.go:525`、`kimi/session.go:368`）、abort 超时（kimi 30 min 默认，`kimi/options.go:42`）、错误抑制（kimi abort 丢弃真实错误，`kimi/session.go:157-159`）、usage 计数（claudecode 丢弃 cache token，`claudecode/session.go:236`）各不相同。 |
| 修复方向 | 参数化抽出 persistent JSON-RPC session 基类（注入 create/resume 方法名、turn-start RPC、终态判定），**强制收敛已漂移行为**；claudecode 因是基于行的控制协议，保持独立。 |
| 工作量 | L · 风险 中（触及三 agent 核心，需完整回归）。 |

---

#### D3. adapter 共享 helper 三份重复

| 项 | 内容 |
|----|------|
| 严重度 | P3 |
| 根因 | `adapters/{codex,claudecode,kimi}` 的 `envOrDefault`/`envBool`/`splitEnv`/`splitCommand`/`newAdapter`/`nativeSession` 接口/`starter` 类型/`mapEvent`/`mapTool`/`promptForNative`/`lastUserMessage` 逐字复制（~50 行 ×3）。另有 helper 漂移：`truncate` 在 codex 是 rune-safe（`codex/protocol.go:120-126`）、在 claudecode 是按字节会切坏 UTF-8（`claudecode/protocol.go:90-95`）。 |
| 修复方向 | 提到 `adapters/internal`；统一 `truncate` 等漂移 helper。 |
| 工作量 | M · 风险 低。 |

---

#### D4. 死代码/死配置 + 超时返回不一致

| 项 | 内容 |
|----|------|
| 严重度 | P3 |
| 根因 | 死配置：`ProfileOverride`（`config/gateway.go:131`）、`Seccomp.ProfileFile`（`:192`）声明但从不读取；`lifecycle_mode`（`supervisor.go:967`、`worker.proto:81`）echo 但 `Abort`（`:633`）从不消费。死代码：codex `Mode`（`codex/options.go:32` 规范化但无人读）、`promptPreamble`（`codex/launch.go:8`）、`prependPreamble`（`codex/protocol.go:113`）、`constantTimeEqual`（`types.go:551`）、`authenticateCaller` 的 `found==1` 守卫（`types.go:329-340`，map key 唯一永不触发）。不一致：非流式超时返回 HTTP 500（`chat_completions.go:632-635`）vs 流式超时返回 `finish_reason:"length"`（`stream.go:301-307`）。锁误用：`isServerToolID`/`getHandle`/`markTurnSettling` 用写锁而 RLock 即足（`types.go:166-175,408-412,118-125`）。 |
| 修复方向 | 删死代码/死配置或补接线；统一超时语义；改用 RLock。 |
| 工作量 | S · 风险 低。 |

---

## 5. 已审计并排除的误报

> 记录此处，防止其他优化方案重复提出，并体现审计严谨性。

| 怀疑点 | 结论 | 证据 |
|--------|------|------|
| `stream.go:333` deferredAbort 在 error 分支未关闭，导致 worker 已报错的 turn 仍触发一次无意义 abort RPC + 31s drain | **误报**。`status` ∈ {`"normal"`,`"error"`,`"timeout"}`；`if status != "timeout" { deferredAbort = false }` 在 `"error"` 时**同样置 false**，即 clean finish 和 error 都正确跳过 abort，仅 timeout 保留 deferred abort（符合注释 stream.go:330-332）。逻辑正确。 | `api/openai/stream.go:284-289,330-335` |
| SessionStore deep copy 是否到位 | **正确**。`SessionRecord`（`runtime/session.go:38`）全为值类型（string/time.Time/status 枚举），赋值即深拷贝，并有 `TestSessionRecordContainsNoProcessPointers` 守护。 | `runtime/session_store.go:114` |
| Registry 是否真正 name-agnostic | **正确**。`runtime/` 全包无任何 agent 名硬编码；adapter 经 `init()` 自注册；archtest 强制 API 层不 import adapters/worker。 | `runtime/registry.go`、`internal/archtest/dependencies_test.go` |

---

## 6. 分阶段路线图

推荐顺序：**A（稳定性）→ B（鲁棒性）→ C（隔离加固）→ D（去重）**。每项独立可合并。

```
Tranche A — 长跑稳定性（最高价值，建议先做）
  A1 Session 记录/Handle TTL + Prune 接线 ............ M, 低风险, 无依赖
  A2 JSON-RPC 读循环解耦 ............................. M, 中风险, 无依赖
  A3 PID namespace（含 C1 跨会话 kill）............... L, 中高风险, 需 Linux-CI 门

Tranche B — 鲁棒性
  B1 stderr 有界 ring buffer ......................... S
  B2 gRPC 消息上限调高 ............................... S
  B3 ws.client/workerPID 加锁 ......................... S
  B4 codex Abort 升级 ................................ M
  B5 terminate 清理顺序（A3 落地后消解）.............. S
  B6 time.After → NewTimer+Stop ...................... S

Tranche C — 隔离加固（A3 已覆盖 C1）
  C2 seccomp 分层校验 ................................ S
  C3 uidmap 默认值对齐 ............................... S
  C4 /etc 挂载集收窄 ................................ S

Tranche D — 架构去重（建议 A/B 稳定后再做）
  D1 统一 agent event 类型（D2 前置）................ M
  D2 抽 JSON-RPC session 基类，收敛行为漂移 ......... L
  D3 提取 adapter 共享 helper ........................ M
  D4 清死代码 + 统一超时语义 ......................... S
```

**可独立合并边界**：除显式标注的依赖（A5←A3、D2←D1）外，各项互不阻塞，可按人力并行或挑选子集执行。

---

## 7. 待决策项

下列取舍需在与其他方案对比后由维护者拍板：

1. **A3 PID namespace 默认开关**：是治硬杀遗漏 CLI 与跨会话 kill 的高杠杆单点，但需 Linux-CI 真机验证（darwin 无法验证）。是否同意改为默认开启、以 CI smoke 通过为门？保留配置开关可回退。
2. **A1 记录 TTL 默认值**：建议 24h（> worker idle 2h，保留 resume）。是否接受？或按业务最长会话间隔调整。
3. **D2 抽基类的时机**：L 级、触及三 agent 核心，回报是收敛已漂移的 abort/close/usage 行为。是否在 A/B 稳定后立即做，还是推迟到有新 Agent 接入需求时再做？
4. **执行粒度**：是按本路线顺序逐 commit/PR 推进，还是挑选特定子集先做（例如只做 A1+A2+B1-B3 这组低风险高回报项）？

---

## 附录：审计覆盖但本次未列单点的次要观察

- `os.RemoveAll` 早返回路径（`supervisor.go:267-287`）在 ValidateBinary/Build 失败时遗留 sessionDir/socketDir（轻微磁盘 litter）。
- `supervisor.Close`（`:449-452`）首个错误即返回，丢弃其余；外层 Close ctx（StopGracePeriod+2s）可能在卡住的 session teardown 完成前到期 → 退出时遗留 worker。
- `shortID`（`supervisor.go:527`）sha256 截断 64 位，碰撞会复用 socket dir 导致 bind 失败，无重试。
- 会话 socket 无 gRPC 鉴权，依赖 0700 socket dir + mount namespace 隐藏（单租户模型下可接受，应显式记为假设）。
- `modelCatalog.has()`（`models.go:65-92`）每个 chat 请求实例化 adapter + Describe（用 `context.Background()`），可缓存。
- kimi 自动审批启发式（`kimi/protocol.go:34-48`）按子串匹配 "allow"/"approve"，措辞不同的选项会在 auto 模式下被拒，静默阻塞。
