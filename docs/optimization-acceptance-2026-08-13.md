# Agent Gateway 优化验收基线

> 制定日期：2026-08-13  
> 验收依据：`docs/optimization-plan-merged-2026-08-13.md` 及 2026-08-13 对齐结论  
> 适用范围：Phase 0-5 的阶段验收、合并前复核和最终发布验收  
> 文档性质：本文件是独立于实现方案的质量合同。实施者可以调整内部实现，但不得降低本文定义的行为、安全、资源和证据门槛。

---

## 1. 验收目标与权威关系

本轮优化的目标不是扩大 OpenAI API 功能面，也不是重新设计 Agent Gateway，而是将现有架构从“功能跑通”推进到“可长期、隔离、受控、可观测地运行”。

文档职责按以下顺序划分：

1. `docs/optimization-plan-merged-2026-08-13.md` 定义问题清单、阶段范围和实施顺序。
2. 本文件定义什么证据足以证明一项工作完成，以及哪些情况必须拒绝验收。
3. `docs/optimization-audit-2026-08-13.md` 仅作为原始问题和源码证据存档。
4. 各 Phase 的详细 implementation plan 可以选择具体实现，但与本文件冲突时，以本文件的验收门槛为准。
5. Phase 4、Phase 5 在选型和设计未单独批准前，只能验收设计或原型，不能宣称整个阶段完成。

验收必须绑定一个不可变 Git commit。工作树、口头说明、局部 diff、开发者本机“曾经通过”的结果都不能作为最终验收基线。

## 2. 不可破坏的产品与架构不变量

以下任一项被破坏，当前 Phase 直接判定为 **不通过**，不能用功能测试通过抵消：

1. Agent CLI 完成完整自治轮次；native `tool_use`/`tool_result` 只属于执行遥测，不映射为 OpenAI `message.tool_calls`、`delta.tool_calls` 或 `finish_reason: tool_calls`。
2. API 进程不直接启动 Agent CLI；执行路径仍为 API -> `runtime.ExecutionBackend` -> Worker -> adapter -> native Agent。
3. `runtime/` 和 `api/openai/` 保持 agent-name-agnostic；`worker/` 不导入具体 adapter；不得以 agent 名称分支代替能力接口。
4. prod/dev 的 nsjail 隔离继续 fail-closed；仅 test mode 可以绕过隔离。配置错误、平台不支持或依赖缺失时不得静默降级到非隔离执行。
5. unknown completion outcome 不得自动重放同一轮 user 输入。
6. 一个 Gateway session 同时最多一个 active turn；不同 session 的生命周期、进程、事件和 resume identity 不得串扰。
7. 客户端只能提交 opaque `workspace_id`，不得将绝对宿主路径或越界路径注入 Worker。
8. unattended OpenAI 请求不得无限等待 permission 或 native reverse request；必须给出确定性响应。
9. Phase 1 仍允许单节点和内存态；在 Phase 4 完成前不得声称支持重启恢复，在 Phase 5 完成前不得声称支持跨节点执行。

## 3. 验收结论与缺陷分级

### 3.1 允许的结论

每个条目和每个 Phase 只能使用以下结论：

- **通过**：全部强制条件满足，证据完整且可复现，无阻断缺陷。
- **有条件通过**：核心行为已满足，仅剩明确记录且不影响安全、正确性、资源有界性和兼容性的 P2/P3 非阻断项；必须给出负责人、截止阶段和复验条件。
- **不通过**：任一强制条件失败，或证据与结论矛盾。
- **无法验收**：缺少必要环境或运行证据，例如没有真实 Linux nsjail/arm64 结果。无法验收不等于通过，也不得用于发布放行。

“代码已写”“单元测试通过”“CI 大部分通过”“理论上成立”不是验收结论。

### 3.2 缺陷等级

- **P0 阻断**：越权 resume、隔离失效、跨 session 串扰、unknown outcome 自动重放、进程无法可靠回收、协议死锁、记录或 handle 持续无界增长。
- **P1 阻断当前 Phase**：公开能力与镜像不符、主要资源上限缺失、并发竞态、超限导致随机断流、关键运行证据缺失、API 过载契约错误。
- **P2 条件项**：不破坏主路径的鲁棒性、兼容性或运维缺口；必须进入后续清单。
- **P3 建议项**：命名、局部重复、非关键性能或文档质量问题。

安全、租户隔离、fail-closed 和 unknown-outcome 问题无论发生概率高低，最低按 P0 处理。

## 4. 验收输入与证据包

实施方申请验收时必须提供以下信息：

| 字段 | 强制要求 |
|---|---|
| 验收 Phase / 优化 ID | 精确到 `Phase N` 和 U1、O-A2 等 ID |
| 基线 commit | 实施前 commit SHA |
| 候选 commit | 待验收 commit SHA，必须可 checkout |
| 变更范围 | `git diff --stat <base>...<candidate>` 和变更文件清单 |
| 决策记录 | 对背压、超限、TTL、timeout、PID namespace、持久化等非平凡语义给出最终选择 |
| 自动化测试 | 测试名称、命令、退出码和完整 CI 链接/日志 |
| 回归证明 | 每个 bug fix 证明测试在 pre-fix 代码失败、在 candidate 通过；或解释为何只能使用等价 fault injection |
| 运行证据 | 压测、进程树、Linux jail、容器权限、arm64 等对应原始输出 |
| 已知限制 | 未覆盖环境、残余风险、延期项及其归属 Phase |

验收人必须独立完成：

1. 确认 candidate commit 与送审内容一致，且没有依赖未提交工作树状态。
2. 阅读完整 `base...candidate` diff，不仅看实施方摘要。
3. 核对每个优化 ID 都有可搜索的回归测试或明确的验收脚本。
4. 在可用环境中重新执行关键命令，不直接采信他人报告或历史日志。
5. 对安全、并发、进程和恢复路径进行负向测试，不能只验证 happy path。

## 5. 全阶段通用硬门禁

### 5.1 源码与依赖方向

- `internal/archtest` 全部通过。
- `runtime/` 只依赖标准库，不出现 `codex`、`claude`、`kimi` 等具体 agent 分支。
- `api/openai/` 不导入 `adapters/*`、`worker/` 或 `config/`。
- `worker/` 不导入 `adapters/*` 或 `api/openai/`。
- adapter 仍然是 thin bridge，不启动进程、不解析原始 JSONL、不管理进程组。
- agent 包不导入 Gateway runtime/HTTP/gRPC 层。

建议独立执行：

```bash
go test ./internal/archtest/ -v -count=1
rg -n 'codex|claude|kimi' runtime api/openai
go list -deps ./runtime/...
```

`rg` 命中注释、测试数据或公共模型字段时需人工判断；命令本身不是零命中的机械门禁。

### 5.2 标准质量命令

候选 commit 至少执行以下命令，并记录日期、平台、Go 版本和退出码：

```bash
git diff --check <base>...<candidate>
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go test ./integration/ -v -count=1
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

此外还必须满足：

- Go 源码已 `gofmt`。
- protobuf 变更只能由仓库生成流程产生，生成文件与 `.proto` 一致。
- 配置变更覆盖默认值、strict TOML、非法值、旧 key 迁移或拒绝行为。
- API/runtime/worker/workspace 改动必须运行集成测试。
- 并发改动必须运行 race detector 和针对该路径的压力/阻塞测试。
- sandbox/profile/process tree 改动必须在真实 Linux 上运行；Darwin mock 结果不能替代。
- 日志、错误响应、测试 fixture 不得泄漏 token、凭据、完整宿主路径或其他 session 数据。

### 5.3 回归测试质量

每个 bug fix 的测试必须满足：

1. 名称可检索到原问题，例如 `ResumeIDCannotBeOverriddenByClientMetadata`。
2. 直接断言外部行为或边界契约，不只断言内部 helper 被调用。
3. 修复前失败原因与原 bug 一致，修复后稳定通过。
4. 并发测试不能只依赖随机调度；应使用 barrier、阻塞 channel、fake clock 或 fault injection 建立确定性窗口。
5. 不以无限 sleep 作为同步；所有等待有明确超时，失败时输出诊断状态。
6. 涉及三种 Agent 的共享契约时，必须覆盖 Codex、Claude Code、Kimi，不能由单一 adapter 测试外推。

### 5.4 最终拒绝条件

出现以下任一情况，无需继续计算测试通过率，直接拒绝当前 Phase：

- 为通过测试而放宽 prod/dev fail-closed。
- 将 native tool telemetry 暴露为 OpenAI tool calls。
- 客户端仍可控制任一 native resume identifier。
- 队列、stderr、tool output、RPC frame、日志或 session 记录中存在已知无界增长路径。
- 通过增大 buffer/timeout/message limit 掩盖背压或超限契约缺失。
- PID namespace、seccomp、容器权限只做静态配置断言，没有要求的真实 Linux 证据。
- race detector、集成测试或跨平台构建失败后仍以“不相关”口头放行，且没有隔离复现和书面裁定。
- 自动重试可能重放 completion outcome 未知的 turn。

## 6. Phase 0 验收：正确性与信任边界

Phase 0 只有在 U1、O-A2、O-F03、O-F09a 全部通过后才能整体通过。O-B1/O-B3 可以独立验收；O-B2 必须按事件大小契约验收，不能以简单调大 gRPC 上限通过。

### 6.1 U1：Session 记录与 Handle 生命周期闭环

**必须满足**

- `record_ttl` 与 Worker `idle_timeout` 是两个独立概念；合法配置保证记录寿命足以覆盖 Worker idle reclaim 后的正常 resume 窗口。
- 每次成功创建、访问、完成 turn 或其他已批准的活跃行为都会按统一规则刷新记录过期时间；失败路径不会制造永久零过期记录。
- prune loop 有明确启动、停止和时钟语义；Gateway shutdown 后不遗留 goroutine。
- 过期记录被物理删除，关联的 API cached handle 和 server tool ID 等 session 附属状态同步释放。
- Worker 自然退出、心跳失败、idle reclaim、显式 Close、启动失败等终态都能让 API 感知 handle 已失效；下一次请求只做已定义的 lazy recovery，不复用 dead handle。
- active turn 不得被 TTL/prune 并发删除；owner scope 和 single-active-turn 约束保持不变。
- TTL 不能是唯一容量治理说明：当前 Phase 至少证明到期后集合不再单调增长；全局/单 caller admission cap 最迟在 Phase 2 完成。

**必需证据**

- fake clock 或可控时钟测试覆盖触达、过期边界、重复 prune、active turn、owner isolation。
- handle `Done()` 与 prune/idle reclaim 竞态测试。
- 至少 1000 个 session 的创建 -> idle -> 过期 -> prune 压测；最终 store、handle map 和相关 session map 回落到稳定基线，无持续单调增长。
- `go test -race` 覆盖 prune、请求和 Worker 终态并发路径。

**不通过示例**

- 只修改 `Prune` 返回值，但没有实际 prune loop 或 API handle 清理消费者。
- 记录被删了，但 `handles`、tool IDs 或 goroutine 仍随历史 session 数增长。
- TTL 默认值被拍定，却没有配置约束、内存证据或恢复语义测试。

### 6.2 O-A2：JSON-RPC 控制流与事件背压隔离

**必须满足**

- notification 展示事件的消费变慢或停止时，JSON-RPC response、reverse request 和连接终止仍能被及时处理。
- 实现必须有界；固定队列容量可以是 256 或其他值，但容量值本身不构成修复。
- 明确定义并测试队列满策略：阻塞、聚合、丢弃可丢事件或受控终止，只能选择有可观察结果的策略。
- `finish`、`error`、permission/reverse request、native session identity 和协议终态不得静默丢失。
- text/reasoning/tool progress 若允许聚合或截断，必须保持必要顺序并带明确 truncation/overflow 信号。
- Close/Abort/进程退出时不发生 send-on-closed-channel、双重 close、goroutine 泄漏或永久 drain。
- Codex 与 Kimi 均覆盖；共享 transport 修复不得错误统一两者的 native lifecycle 语义。

**必需证据**

- 确定性测试主动阻塞事件消费者并填满 mailbox，随后证明一个独立 RPC response 仍在限定时间内完成。
- overflow 策略测试覆盖不可丢事件和顺序。
- Close 与 notification flood、Abort 与 response 同时发生的 race 测试。
- goroutine/资源稳定性测试，不接受仅把 channel 从 64 改成 256。

### 6.3 O-F03：Native Resume ID 归服务端所有

**必须满足**

- 外部请求 metadata 中的 `native_session_id`、`codex_thread_id`、`claude_session_id`、`kimi_session_id` 及所有等价别名均不能选择或覆盖 native resume 目标。
- Gateway 内部保存的 native ID 与客户端 metadata 在类型或信任边界上可区分；不能依赖“同一个 map 中服务端稍后覆盖”的脆弱顺序。
- Codex、Claude Code、Kimi 行为一致。
- resume 仍限定在原 caller、Gateway session、model/adapter 和 workspace 边界内；跨 owner 或不匹配 model/workspace 必须拒绝。
- 错误和日志不得回显其他 session 的 native ID。

**必需证据**

- 三 adapter table-driven 负向测试，分别注入每个保留 key 和冲突组合。
- HTTP 集成测试证明客户端注入不能到达 backend/adapter。
- 跨 caller、跨 Gateway session、跨 model/workspace 的越权恢复测试。
- 正常 Worker crash -> saved server-owned ID -> next request resume 路径仍通过。

### 6.4 O-F09a：Timeout 配置禁止静默接受

**必须满足**

- 所有可配置字段都真实生效；尚未支持的 agent timeout 配置必须在启动/解析时明确拒绝，或字段明确命名为 agent-specific。
- 不允许配置被接受、记录或传入 RPC 后仅对 Kimi 生效而 Codex/Claude 静默忽略。
- Phase 0 可以不完成三层 deadline 统一，但必须维持 unknown-outcome 不重试。

**必需证据**

- Codex、Claude、Kimi 的配置矩阵测试。
- 非法/不支持组合的 strict config 负向测试。
- 超时后不自动重发相同 user turn 的集成测试。

### 6.5 O-B1：stderr 有界保留

**必须满足**

- stderr 内存占用有明确字节上限，保留最近内容而不是最早内容。
- 截断结果带 marker，UTF-8 边界不会产生不可控乱码或 panic。
- 并发 Write/String/Close 安全；错误信息仍有足够尾部诊断内容。

**必需证据**

- 写入远超上限的数据后长度有界且包含末尾 sentinel。
- 多 goroutine 写读 race 测试。
- native CLI 失败时错误仍引用 bounded tail。

### 6.6 O-B2：Canonical Event 与 RPC Frame 大小契约

**必须满足**

- 在 agent/adapter/runtime 进入 RPC 前存在单事件大小或各字段大小上限。
- 大型 tool output 使用有界、脱敏、可识别截断的展示形式；不能只把 gRPC 上限从 4 MiB 调到更大。
- gRPC send/recv 上限略高于 canonical event 上限，作为防御边界；server 与 client 配置一致。
- 超限产生确定的 truncated event 或 typed error，不导致无说明的 session 随机断流。
- 并发大事件不能导致不受控内存放大。

**必需证据**

- 边界值测试：上限内、恰好上限、超限、极端超限。
- tool result、text、reasoning、error 等可能承载大内容的路径测试。
- gRPC 真实 stream 集成测试和内存/分配观测。

### 6.7 O-B3：Worker Session 状态安全发布

**必须满足**

- `client`、`workerPID` 及相关 handshake state 通过明确 publication point、mutex snapshot 或等价机制安全发布。
- heartbeat/bridge 不得在 handshake state 未完整发布时访问半初始化状态。
- 不在持锁状态执行可能阻塞的 RPC、Wait、Close 或 channel drain。
- terminate、heartbeat、killGroup 和 start failure 并发时幂等、无 panic、无泄漏。

**必需证据**

- 使用 barrier 强制 handshake/heartbeat/terminate 交错的确定性测试。
- `go test -race` 覆盖该测试；不能只以 race detector 偶然未报错作为修复证明。

### 6.8 Phase 0 出口

- U1、O-A2、O-F03、O-F09a 全部通过。
- 纳入 Phase 0 candidate 的 O-B1/O-B2/O-B3 各自完整通过，不能称为“顺手修改”而降低证据要求。
- 全阶段通用门禁通过。
- 千级 session 压测无 store、handle、goroutine、Worker/CLI 进程或临时目录单调增长。
- 恶意 metadata、阻塞消费者、Worker crash、timeout 和并发 prune 的负向路径全部有自动化证据。

## 7. Phase 1 验收：发布与沙箱声明可证明

### 7.1 O-F04/U3：Agent 可用性与模型目录

- `/v1/models` 只广告当前部署真实可执行、配置有效且 readiness probe 通过的 Agent。
- 镜像内 CLI 版本/路径与配置契约一致；缺 CLI、不可执行、版本不支持时不广告，并给出不泄密的结构化诊断。
- descriptor 缓存不在每次请求实例化 adapter；缓存有明确初始化/失效语义，配置变化不会永久返回陈旧状态。
- readiness 与 model discovery 的失败语义一致，单个 Agent 不可用是否影响全局 readiness 必须有批准的明确策略。
- 必须使用实际镜像/CLI stub 组合验证“存在、缺失、坏版本、探针超时”，不能只 mock `Describe()`。

### 7.2 O-F05：非特权 nsjail 安全矩阵

- 真实 Linux 容器以目标 uid（当前发布约定为 65532 或最终批准值）、`cap-drop=ALL`、`no-new-privileges` 运行完整最小 jail。
- smoke 不得依赖 `--privileged` 来证明生产声明；若 CI 平台确需额外能力，必须列出生产所需的最小 capability/security option，并验证完全一致的部署 tuple。
- workspace、agent home、tmp、socket 挂载权限正确；宿主其他路径不可见/不可写。
- 缺 nsjail、版本不符、namespace 创建失败时 readiness/start fail-closed。
- 验收证据包含容器启动参数、uid/gid/capabilities、mount namespace 和实际执行输出。

### 7.3 O-A3：每会话 PID 隔离与整树回收

- PID namespace 是首选候选方案，但只有真实 Linux 验证满足全部行为后才可判定通过。
- 每个 session 不能看见或向其他 session 的 Worker/CLI 发信号；负向 kill 测试必须失败。
- graceful stop、Abort escalation、Worker crash、nsjail crash、Gateway shutdown、SIGKILL 均不遗留 Worker、CLI 或 CLI 后代。
- 必须验证 `nsjail -Mo`、namespace PID 1、`/proc`、PTY、子进程、Tini/user namespace 的实际组合行为。
- 若 PID namespace 被确认是必要安全边界，prod/dev 不得通过普通配置静默关闭；test mode 可以关闭。若方案验证失败，必须改用经过验收的 cgroup v2 或显式 descendant tracking，不能以共享 PID namespace 放行。

### 7.4 O-F06：Seccomp 架构与真实执行

- amd64、arm64 使用正确的 syscall policy；未知架构 fail-closed。
- 交叉编译只能证明 build，不能替代目标架构上的真实 jail + Worker + CLI/stub smoke。
- policy 拒绝项和允许项均有运行测试；不得通过关闭 seccomp 让 smoke 通过。
- policy 变更记录必要 syscall 的来源，避免宽泛放行。

### 7.5 O-F07：HTTP Listener 加固

- `ReadHeaderTimeout`、合理的 header 上限和 request body 上限生效。
- 超大 body 返回稳定的 413/OpenAI-compatible error；慢 header 不无限占用连接。
- SSE 长连接不被不合理的全局短 `WriteTimeout` 误杀；shutdown drain 仍按既有契约完成。
- `/health/*`、`/v1/models`、stream/non-stream chat、abort 均有回归。

### 7.6 O-C2/O-C3/O-C4：隔离配置纵深防御

- seccomp-off 在 config、supervisor/nsjail 边界均 fail-closed，不能通过直接构造内部 config 绕过。
- uid/gid map 默认值与实际镜像运行身份一致，或必须显式配置且启动时验证。
- `/etc` 只暴露 Agent 运行必要文件；若仍整目录只读挂载，必须给出风险评估和批准记录，不能自动判定完成。
- profile golden tests 与真实 Linux smoke 同时通过。

### 7.7 Phase 1 出口

- O-F04、O-F05、O-A3、O-F06、O-F07 全部通过。
- O-C2/C3/C4 与 U3 若纳入本阶段，逐项通过。
- 存在至少一套与生产声明一致的非特权 Linux 验证结果。
- arm64 没有真实运行证据时，arm64 发布能力只能判定“无法验收”，不得由 amd64 结果推导。

## 8. Phase 2 验收：Run 治理、观测与资源上限

### 8.1 O-F11：Run 身份与观测

- 每次 turn 有全局唯一、稳定且不可由客户端覆盖的 Run ID；response header、stream、日志、指标和持久/内存记录可关联同一 Run。
- 日志至少包含 caller、Gateway session、Run、model、workspace opaque ID、Worker 状态和终态类别；不得输出 token、完整 prompt/tool secret、native credential 或宿主路径。
- 指标覆盖 active/queued runs、latency、terminal status、abort、timeout、worker crash、unknown outcome、reclaim 和 admission reject。
- 指标 label 不使用高基数 session/run/user 原值。
- trace/context 能跨 HTTP -> backend -> Worker RPC，但 context cancellation 不导致 unknown outcome 自动重试。

### 8.2 O-F10：准入控制、配额与过载契约

- 至少具备全局、per-caller、per-agent 并发/排队上限；workspace/log/tmp/event 资源有明确上限或基础设施强制契约。
- 达到上限时快速、确定地返回 429 或批准的服务繁忙错误，包含合理 retry signal；不得先创建 Worker 再拒绝。
- 公平性策略能阻止单 caller 占满全部 slot；取消、超时和 crash 后配额必定归还。
- admission、session single-active-turn 和 Worker `MaxConcurrency` 语义不重复计数、不死锁。
- 压测覆盖一个恶意 caller、多个正常 caller、排队取消、Worker crash 和 Gateway shutdown。

### 8.3 O-B4/B5/B6：Abort、清理和 Timer

- Codex 在 turn ID 尚未发布时收到 Abort，结果必须确定：等待安全 publication、协议级取消或受控 teardown；不得永久失败或误取消下一轮。
- terminate 先确认进程树退出，再删除其依赖的 socket/profile/session 目录；并发多次 terminate 幂等。
- timer 使用可停止/回收机制，循环执行不积累 timer/goroutine。
- Claude 无可靠 interrupt 时允许 teardown + saved native ID resume，但必须标记 unknown outcome，不自动重放。

### 8.4 Phase 2 出口

- O-F10、O-F11 通过，过载与可观测性具备端到端运行证据。
- O-B4/B5/B6 通过；进程、slot、timer、日志和临时目录在故障压测后回落。
- Phase 0 延期的 global/per-caller session record cap 在本阶段必须完成，否则 U1 只能维持“有条件通过”。

## 9. Phase 3 验收：Typed 配置、Deadline 与审慎去重

### 9.1 O-F08：Typed 配置直达 Adapter

- config -> runtime/RPC -> Worker -> adapter 的关键字段保持 typed，不再通过 `CC_GATEWAY_*` 环境变量序列化后重新解析。
- Agent CLI 真正需要的环境变量只能在 native process launch 边界生成，并有 secret redaction。
- registry/factory 签名保持 name-agnostic；不得在 worker/runtime 中加入 agent 名分支。
- 默认值、非法配置、三 Agent 配置传递和 RPC round-trip 有测试。

### 9.2 O-F14：Command argv 化

- command 使用 argv 数组或等价结构，不经过 shell，也不使用 `strings.Fields` 解释引号。
- 可执行路径和参数包含空格、引号、反斜杠时保持逐参数语义。
- 旧字符串配置若保留一个版本，必须给出明确 deprecation、无歧义迁移和拒绝规则；不得自行实现不完整 shell parser。

### 9.3 O-F09b：统一 Deadline 契约

- 明确区分 HTTP transport deadline、Gateway run deadline、native operation deadline 的所有权和优先级。
- stream/non-stream 对同一终态使用语义一致的 HTTP/SSE/OpenAI 表达；不得一边 500、一边伪装正常 `finish_reason:length`。
- timeout、client cancellation、protocol error、worker crash、unknown outcome 可区分并进入日志/指标。
- timeout 不等于 token length；不得自动重放完成结果未知的 turn。
- Codex、Claude、Kimi 使用统一 Gateway 契约，同时保留 native abort 能力差异。

### 9.4 D1/D2/D3/U4：去重验收护栏

- 先存在三 Agent 行为矩阵和 parity tests，再进行公共抽取。
- 只提取契约逐字一致且稳定的 primitive；native initialize、resume、turn completion、abort、usage、reverse request 差异仍由各 agent 包拥有。
- `agent/*` 不导入 `runtime.Event`；共享 agent-internal 类型不得抹平 native 信息。
- D2 JSON-RPC session 基类不是必做结果。若抽取后条件分支、接口复杂度或跨包耦合上升，应判定该抽象不通过并保留独立 lifecycle。
- adapter helper 抽取后 Codex/Claude/Kimi 的 prompt、event、permission、usage、finish mapping parity 全部保持。
- 以减少行为漂移和认知复杂度为验收目标，不以删除行数作为通过指标。

### 9.5 U2：死代码与鉴权语义

- 删除未使用代码和无效配置后，全仓无引用、配置示例和文档残留。
- token 比较仍保留 constant-time 语义；不得因为删除 `constantTimeEqual` 而退化为普通字符串比较或直接 map lookup。
- 鉴权成功/失败、不同长度 token、多个 caller token 有测试，日志不泄漏 token。

### 9.6 Phase 3 出口

- O-F08、O-F14、O-F09b、U2 通过。
- D1/D2/D3 逐项独立裁定；D2 被明确评估为“不应抽取”不阻塞 Phase 3，但必须有行为矩阵和书面结论。
- 架构测试、三 Agent native fake tests、adapter parity tests 和 HTTP integration 全部通过。

## 10. Phase 4 验收：持久化与幂等

Phase 4 必须先提交独立设计并批准存储选型、数据模型、一致性和迁移方案。仅“接入 PostgreSQL/Redis”不能通过验收。

**必须满足**

- SessionRecord、RunRecord、server-owned native identity、owner/model/workspace binding 和终态可在 Gateway 重启后恢复。
- 明确定义 source of truth、事务边界、TTL/容量清理、schema version 和迁移/回滚策略。
- 同一 session 的 single-active-turn 在多进程竞争下仍成立，不能只靠进程内 mutex。
- 幂等键与 Run ID 行为明确；重复请求不得在 completion outcome 已知时重复执行，也不得对 unknown outcome 自动假定成功或重放。
- 加密、访问控制、备份、恢复和 secret/PII 保留策略明确。
- store 暂时不可用、慢响应、部分写入、Gateway crash、Worker crash、网络分区均有 fault-injection 结果。
- 从内存 store 迁移不改变 `api/openai` 与 adapter 契约。

**发布证据**

- Gateway 重启恢复、并发 leader/claim、TTL prune、schema upgrade/downgrade、备份恢复测试。
- 至少一次 kill -9 故障矩阵，核对每个 Run 最终状态和是否允许继续。
- 数据库/Redis 断连时 fail behavior 明确，不产生两个 active turn。

## 11. Phase 5 验收：远程执行与横向扩展

Phase 5 必须先批准调度、workspace、身份和故障域设计。`ExecutionBackend` 能远程调用不等于支持分布式生产部署。

**必须满足**

- API 节点不依赖本地 Worker/CLI，可通过受认证、加密的 remote backend 调度。
- session ownership/lease、Worker fencing、重复调度防护和节点失联恢复明确。
- 同一 session 在任意时刻只有一个有效执行 owner；旧 owner 恢复后不能继续写入事件或状态。
- workspace 策略明确且经过批准：共享存储、同步快照或节点亲和性之一；opaque `workspace_id` 和越界防护保持。
- server-owned native identity 不跨 caller/model/workspace 泄漏。
- 网络分区、调度超时、Worker 节点 crash、API 节点 crash、重复消息和乱序事件有故障测试。
- unknown outcome 原则在跨节点环境继续成立；调度层不得透明重放 turn。
- 指标和 trace 能跨节点关联，但不使用高基数身份作为 metrics label。

**发布证据**

- 至少两 API 节点、两 Worker 节点的真实端到端测试。
- lease/fencing、节点滚动升级、故障转移、workspace 一致性和容量压测。
- 单节点模式兼容回归，且 API/runtime 不出现 concrete backend/agent 分支。

## 12. 优化 ID 完整性矩阵

最终验收必须逐行给出状态和证据链接，不允许只按 Phase 总结：

| ID | 最低验收阶段 | 核心证据 | 状态 |
|---|---:|---|---|
| U1 | 0/2 | TTL + prune + handle Done + 千级 session 稳态 + Phase 2 caps | 待验收 |
| O-A2 | 0 | stalled consumer 下 RPC 活性 + overflow/终态契约 | 待验收 |
| O-F03 | 0 | 三 Agent metadata 注入与跨 owner 负向测试 | 待验收 |
| O-F09a | 0 | timeout 配置矩阵，无 silent accept | 待验收 |
| O-B1 | 0 或 2 | stderr bounded tail + race | 待验收 |
| O-B2 | 0 或 2 | canonical event size + RPC guard + 大事件集成 | 待验收 |
| O-B3 | 0 | handshake publication + deterministic race test | 待验收 |
| O-F04/U3 | 1 | 实际镜像可用性与目录缓存失效 | 待验收 |
| O-F05 | 1 | 非特权真实 Linux jail | 待验收 |
| O-A3 | 1 | PID 隔离与整树 kill 矩阵 | 待验收 |
| O-F06 | 1 | amd64/arm64 真实 seccomp smoke | 待验收 |
| O-F07 | 1 | slow header/body limit/SSE 回归 | 待验收 |
| O-C2 | 1 | 分层 seccomp fail-closed | 待验收 |
| O-C3 | 1 | uidmap 与镜像身份一致 | 待验收 |
| O-C4 | 1 | `/etc` 最小暴露或批准的风险记录 | 待验收 |
| O-F11 | 2 | Run ID + logs/metrics/trace + unknown outcome | 待验收 |
| O-F10 | 2 | admission/fairness/429/capacity stress | 待验收 |
| O-B4 | 2 | Codex pre-turn-id Abort 竞态 | 待验收 |
| O-B5 | 2 | process exit before directory cleanup | 待验收 |
| O-B6 | 2 | timer/goroutine 稳态 | 待验收 |
| O-F08 | 3 | typed config round-trip，无 env 往返 | 待验收 |
| O-F14 | 3 | argv 特殊字符测试，无 shell | 待验收 |
| O-F09b | 3 | 三层 deadline + stream/non-stream parity | 待验收 |
| D1/D2/D3/U4 | 3 | behavior matrix + parity + 架构护栏 | 待验收 |
| U2 | 3 | dead code scan + constant-time auth | 待验收 |
| O-F12 | 4 | restart recovery + distributed claim/idempotency | 待验收 |
| Remote backend | 5 | lease/fencing/workspace/fault matrix | 待验收 |

合并文档附录中的观察项也必须在最终报告中逐项标记“已修复、仍存在但接受、已证明误报”之一：

- `RemoveAll` 早返回遗留目录
- `Supervisor.Close` 首错即返
- `shortID` 碰撞无重试
- Kimi 自动审批子串匹配启发式

未被列入当前 Phase 的观察项不阻塞阶段验收，但不能在最终全量验收中无结论消失。

## 13. 最终全量验收流程

最终验收按以下顺序进行，前一层失败时停止发布判定，但继续记录后续审查发现：

1. **基线冻结**：记录 base/candidate SHA、Go/OS/arch、配置样例、容器镜像 digest 和 native CLI 版本。
2. **范围审计**：检查完整 diff、依赖方向、生成文件、配置迁移和文档同步。
3. **逐 ID 代码审查**：按第 12 节矩阵核对实现与回归测试，不接受批量“已完成”。
4. **标准验证**：执行第 5.2 节全部命令。
5. **故障与压力验证**：session churn、blocked consumer、大事件、abort/timeout、Worker/CLI crash、shutdown、配额、持久化/网络故障。
6. **Linux 安全验证**：非特权 nsjail、PID namespace、seccomp amd64/arm64、mount/uidmap、整树清理。
7. **不变量复核**：重新检查第 2 节，尤其是 native tools、unknown outcome、server-owned resume ID 和 fail-closed。
8. **残余风险裁定**：每项给出等级、影响、owner、截止日期/Phase；P0/P1 不得带病通过。
9. **签发报告**：输出通过/有条件通过/不通过/无法验收，并附所有证据索引。

## 14. 验收报告模板

```markdown
# Agent Gateway 优化验收报告

- 验收日期：
- 验收人：
- 验收范围：Phase / IDs
- Base commit：
- Candidate commit：
- 镜像 digest：
- Go / OS / Arch：
- Native CLI 版本：

## 总结论

结论：通过 / 有条件通过 / 不通过 / 无法验收

一句话依据：

## 不变量检查

| 不变量 | 结果 | 证据 |
|---|---|---|
| Native tools 不映射为 OpenAI tool calls | | |
| API 不直接启动 CLI | | |
| runtime/API name-agnostic | | |
| prod/dev isolation fail-closed | | |
| unknown outcome 不自动重放 | | |
| owner/session/workspace 隔离 | | |

## 优化项结果

| ID | 结论 | 源码证据 | 测试/运行证据 | 残余风险 |
|---|---|---|---|---|
| | | | | |

## 命令结果

| 命令 | 环境 | 退出码 | 日志链接/摘要 |
|---|---|---:|---|
| `go test ./... -count=1` | | | |
| `go test -race ./... -count=1` | | | |
| `go vet ./...` | | | |
| `go test ./integration/ -v -count=1` | | | |
| Linux arm64 build | | | |
| Windows amd64 build | | | |
| 非特权 nsjail smoke | | | |
| amd64/arm64 seccomp smoke | | | |

## 压力与故障矩阵

| 场景 | 预期 | 实际 | 证据 | 结论 |
|---|---|---|---|---|
| 千级 session churn | 资源回落，无单调增长 | | | |
| 事件消费者阻塞 | RPC 控制流继续 | | | |
| 超大 tool output | 有界截断或 typed error | | | |
| Abort/timeout | 终态明确，不重放 | | | |
| Worker/CLI crash | 整树清理，可控恢复 | | | |
| Gateway shutdown | drain 后无遗留 | | | |
| 恶意 caller 抢占 | 公平拒绝/429 | | | |

## 缺陷与延期项

| 编号 | 等级 | 影响 | 是否阻断 | Owner | 复验条件 |
|---|---|---|---|---|---|
| | | | | | |

## 最终签字

- [ ] 所有 P0/P1 已关闭
- [ ] 所有必要 Linux/arm64 证据已取得
- [ ] 每个 bug fix 有 red-green 回归证明
- [ ] 文档、配置示例和发布声明与实际一致
- [ ] Candidate commit 与验收 commit 完全一致
```

## 15. 本文档的变更规则

验收期间可以补充测试细节、环境命令和证据链接，但不得由实施者单方面降低以下门槛：

- 架构不变量
- 安全和租户隔离负向测试
- 资源有界性
- unknown outcome 不重放
- 真实 Linux/nsjail/arm64 运行证据
- bug fix 的 red-green 回归证明
- candidate commit 可复现性

如果实现发现原首选方案不可行，应先更新决策记录，说明替代方案如何满足同一验收结果；不得通过删除或模糊验收条件宣告完成。
