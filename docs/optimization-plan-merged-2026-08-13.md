# Agent Gateway 优化总纲（合并版）

> 合并日期：2026-08-13
> 合并来源：
> - 《代码审计与优化方案》`docs/optimization-audit-2026-08-13.md`（下称 **AUDIT**，A1–D4 编号，基线 commit `ffaf107e`）
> - 《优化路线图》`docs/superpowers/plans/2026-08-13-agent-gateway-optimization-roadmap.md`（下称 **ROADMAP**，F-01–F-15 编号，Phase 0–5）
> 合并方法：逐项源码交叉验证（`file:line` 抽检），去重、统一严重度、消解冲突、保留双方独有发现。
> 文档用途：**作为后续实施的唯一权威计划**。两份源文档保留为证据存档，本文档取代其执行地位。

---

## 1. 执行摘要

`agent-cli-gateway` 架构骨架扎实（分层由 archtest 强制、测试覆盖真实子进程/信号/gRPC），但两份文档共同确认：项目处于 **"从能跑通到能长跑"的拐点**。当前债务集中在四类：

1. **长跑稳定性**（最高优先）：session 记录/handle 无界增长（内存泄漏，双方一致）；JSON-RPC 读循环被满 event channel 阻塞（活性风险，AUDIT 独有但源码已证实）；**客户端可通过 metadata 选择任意 native resume 目标**（信任边界 bug，ROADMAP 独有但源码已证实）。
2. **隔离与进程治理**：硬杀路径遗漏 CLI 独立进程组 + 跨会话 kill 向量，AUDIT 用 PID namespace 一招解两题；ROADMAP 补充了"文档安全声明未被 CI 证明"（`--privileged` smoke vs 非 root 声明）与 seccomp 架构错配。
3. **生产化缺口**（ROADMAP 主体）：模型发现与镜像实际内容脱节、HTTP 监听无加固、无 Run 身份/指标/准入控制/配额、内存态无法跨重启。
4. **可维护性**：三层 agent event 类型 / adapter helper / codex-kimi JSON-RPC 生命周期大量复制且已**行为漂移**；死代码死配置若干。

**统一判断**：先闭环正确性与信任边界（AUDIT Tranche A + ROADMAP Phase 0，双方高度重合），再让发布声明可被 CI 证明（Phase 1），然后治理与观测（Phase 2），去重放到行为边界被测试保护之后（Phase 3），持久化与横向扩展最后（Phase 4/5）。

---

## 2. 交叉验证结果

### 2.1 双方一致发现（高置信，合并后唯一编号）

| 合并 ID | 主题 | AUDIT | ROADMAP | 合并裁定 |
|---------|------|-------|---------|----------|
| **U1** | Session 记录 & Handle 无界增长 | A1 (P0) | F-01/F-02 (Phase 0) | **P0**。AUDIT 给了根因（`ExpiresAt` 恒零值、`Prune`/`Touch` 死代码）；ROADMAP 给了更完整的修复契约（`record_ttl` 与 `idle_timeout` 分离 + `ExecutionHandle.Done()` 终态通知）。采用 ROADMAP 的契约 + AUDIT 的证据。 |
| **U2** | 死代码/死配置清理 | D4 | F-15 | **P3**。`constantTimeEqual` 两边都点名；合并执行。注意 ROADMAP 明确要求**保留** constant-time token 校验语义（不得退化为普通 map 查找）。 |
| **U3** | modelCatalog 每请求重复实例化 adapter | 附录观察 | F-15 | **P3**。合并为"静态描述缓存 + 显式失效"。 |
| **U4** | adapter/agent 代码重复 | D1/D2/D3 | F-13 | **P3**。AUDIT 颗粒度更细（event 类型 / JSON-RPC 生命周期 / helper 三层分开），ROADMAP 加了护栏（只提取"契约上逐字节相同"的 helper，native 协议逻辑留在各包内）。采用 AUDIT 的拆分 + ROADMAP 的护栏。 |

### 2.2 AUDIT 独有发现（ROADMAP 未覆盖，全部采纳）

| 合并 ID | 原编号 | 主题 | 严重度 |
|---------|--------|------|--------|
| **O-A2** | A2 | JSON-RPC readLoop inline `notify`，消费方背压传导卡死整条连接 | **P0** |
| **O-A3** | A3(+C1) | 每会话 PID namespace：治硬杀遗漏 CLI + 跨会话 kill | **P0** |
| **O-B1** | B1 | agent/process stderr 无界缓冲 | P1 |
| **O-B2** | B2 | gRPC 默认 4 MiB 消息上限，出站 EventFrame 无界 | P1 |
| **O-B3** | B3 | `ws.client`/`ws.workerPID` 无锁读写（data race） | P1 |
| **O-B4** | B4 | codex Abort 未拿到 turn id 时硬失败 | P1 |
| **O-B5** | B5 | `terminate` 在进程组退出前 RemoveAll 目录（O-A3 落地后消解） | P1 |
| **O-B6** | B6 | 热循环 `time.After` 未 Stop（timer 泄漏） | P2 |
| **O-C2** | C2 | seccomp-off 仅在 config 层校验，supervisor/nsjail 层缺 fail-closed | P2 |
| **O-C3** | C3 | uidmap 默认值 1000 vs 容器 65532 部署 foot-gun | P2 |
| **O-C4** | C4 | `/etc` 整目录只读挂载暴露面偏大 | P3 |
| — | 附录 | RemoveAll 早返回遗留目录 / `supervisor.Close` 首错即返 / `shortID` 碰撞无重试 / kimi 自动审批子串匹配启发式 | P3 观察 |

### 2.3 ROADMAP 独有发现（AUDIT 未覆盖，全部采纳）

| 合并 ID | 原编号 | 主题 | 严重度 | 阶段 |
|---------|--------|------|--------|------|
| **O-F03** | F-03 | adapter metadata key（`codex_thread_id` 等）优先于 server-owned `native_session_id` —— **信任边界 bug** | **P0** | Phase 0 |
| **O-F04** | F-04 | 镜像未装 CLI 但 `Describe` 恒成功，`/v1/models` 可广告不可用 Agent | P1 | Phase 1 |
| **O-F05** | F-05 | CI smoke 用 `--privileged`，与文档的非 root/cap-drop=ALL 声明矛盾 | P1 | Phase 1 |
| **O-F06** | F-06 | Kafel policy 仅 x86_64，arm64 只验证了交叉编译 | P1 | Phase 1 |
| **O-F07** | F-07 | `http.Server` 无 ReadHeaderTimeout/MaxHeaderBytes 等加固 | P1 | Phase 1 |
| **O-F08** | F-08 | typed 配置 → `CC_GATEWAY_*` env → 再解析的往返 | P3 | Phase 3 |
| **O-F09** | F-09 | `agents.<id>.timeout` 只有 Kimi 生效，其余静默忽略 | P1 | Phase 0(止血)/3(统一契约) |
| **O-F10** | F-10 | 无准入控制/工作区配额/日志上限/过载契约 | P1 | Phase 2 |
| **O-F11** | F-11 | 无稳定 Run ID/指标/trace/unknown-outcome 计数 | P1 | Phase 2 |
| **O-F12** | F-12 | session/native 映射纯内存，重启丢失、无法多副本 | P2 | Phase 4 |
| **O-F14** | F-14 | 命令用 `strings.Fields` 分词，含空格/引号路径不可表达 | P2 | Phase 3 |

### 2.4 冲突点与裁定

| # | 冲突 | 裁定 |
|---|------|------|
| 1 | **A1 记录 TTL 默认值**：AUDIT 建议 24h；ROADMAP TOML 示例写 `record_ttl = "168h"` | 采用 **168h（7d）**，并在配置注释中写明"记录寿命必须 > worker idle_timeout（2h）以保留 resume 能力"。24h 与 168h 都满足该约束，168h 对低频长会话用户更友好；最终以 Phase 0 实施计划里的决策为准，二者皆可接受。 |
| 2 | **A3 PID namespace**：ROADMAP 完全没有此项 | **采纳**，但挂载到 ROADMAP Phase 1 的"production nsjail security matrix"（O-F05/O-F06）一起做——因为二者都需要 Linux-CI 真机门。default-flip 以 smoke 断言（杀 nsjail → namespace 内整树死亡）通过为准，保留 `clone_newpid` 配置开关可回退。 |
| 3 | **B5（terminate 清理顺序）**：AUDIT 列为独立修复 | 维持 S 级独立修复，但标注"O-A3 落地后自然消解"，不阻塞。 |
| 4 | **D2 抽 JSON-RPC 基类 vs ROADMAP §3.4 只提取 helper**：ROADMAP 明确只提取"契约相同"的部分 | 两者不矛盾：D2 收敛的是 **codex/kimi 的 session 生命周期**（Start/initialize/monitor/Close，且强制对齐已漂移的 abort/close/usage 语义）；§3.4 提取的是 **adapter 映射 helper**。分两个任务执行，D2 需 D1 前置。 |
| 5 | **超时语义**：AUDIT D4 发现"非流式 500 vs 流式 finish_reason:length"不一致；ROADMAP F-09 发现"timeout 配置只有 Kimi 生效" | 合并为一个工作项"**统一 turn 超时契约**"：Phase 0 止血（静默忽略的配置必须拒绝或改名，不允许 silent accept），Phase 3 定义三类 deadline（HTTP 传输 / Gateway run / native 操作）并统一非流式与流式的超时表现。 |

### 2.5 误报登记（双方均已排除，防止再次提出）

| 怀疑点 | 结论 | 证据 |
|--------|------|------|
| `stream.go` deferredAbort 在 error 分支未关闭 | 误报。`status != "timeout"` 时 error 同样置 false，仅 timeout 保留 deferred abort，逻辑正确 | `api/openai/stream.go:284-289,330-335` |
| SessionStore deep copy 是否到位 | 正确。`SessionRecord` 全值类型，有守护测试 | `runtime/session_store.go:114` |
| Registry 是否 name-agnostic | 正确。archtest 强制 | `runtime/registry.go`、`internal/archtest/dependencies_test.go` |

### 2.6 本次合并新增源码抽检记录（2026-08-13）

- **O-A2 证实**：`agent/protocol/jsonrpc.go:160-163` readLoop 内确实 inline 调 `c.notify(msg)`，无队列解耦。
- **O-F03 证实**：`adapters/codex/adapter.go:88-90` `codex_thread_id` 优先、空时回落 `native_session_id`；`adapters/claudecode/adapter.go:104`、`adapters/kimi/adapter.go:104` 直接读 `native_session_id` metadata —— 客户端可控 resume 目标属实，且三 adapter 行为不一致。
- **U1 证实**：`runtime/session_store.go:69-78` Touch/Prune 契约存在；`profile.go:103` `clone_newpid: false` 硬编码（非配置项，O-A3 需要先把它变成可配置）。

---

## 3. 合并后总矩阵

| ID | 发现 | 类别 | 严重度 | 工作量 | 风险 | 归属阶段 |
|----|------|------|--------|--------|------|----------|
| U1 | Session 记录/Handle 无界增长（TTL + Done 通知） | 稳定性 | **P0** | M | 低 | Phase 0 |
| O-A2 | JSON-RPC 读循环解耦（有界队列 + drain goroutine） | 稳定性 | **P0** | M | 中 | Phase 0 |
| O-F03 | native resume ID 收归 server-owned，剥离保留 metadata key | 安全 | **P0** | M | 低 | Phase 0 |
| O-F09a | timeout 配置止血：禁止静默忽略 | 正确性 | **P0** | S | 低 | Phase 0 |
| O-A3 | 每会话 PID namespace（治硬杀遗漏 + 跨会话 kill） | 隔离 | **P0** | L | 中高 | Phase 1（随安全矩阵） |
| O-B1 | stderr 有界 ring buffer | 鲁棒性 | P1 | S | 低 | Phase 0 顺手/Phase 2 |
| O-B2 | gRPC MaxSend/RecvMsgSize 调高 | 鲁棒性 | P1 | S | 低 | Phase 0 顺手/Phase 2 |
| O-B3 | ws.client/workerPID 加锁 | 鲁棒性 | P1 | S | 低 | Phase 0 顺手 |
| O-B4 | codex Abort 无 turn id 兜底 | 鲁棒性 | P1 | M | 中 | Phase 2 |
| O-B5 | terminate 清理顺序（O-A3 后消解） | 鲁棒性 | P1 | S | 低 | Phase 2 |
| O-F04 | Agent 可用性探针 + 模型目录缓存 + 镜像契约 | 发布可信 | P1 | M | 中 | Phase 1 |
| O-F05 | 非特权 nsjail CI 安全矩阵 | 发布可信 | P1 | M | 中 | Phase 1 |
| O-F06 | seccomp 架构感知 + arm64 真机 smoke | 发布可信 | P1 | M | 中 | Phase 1 |
| O-F07 | HTTP listener 加固（不含短 WriteTimeout） | 加固 | P1 | S | 低 | Phase 1 |
| O-F10 | 准入控制 + 配额 + 429 契约 | 治理 | P1 | M | 中 | Phase 2 |
| O-F11 | Run 身份 + 指标 + trace + 结构化日志 | 观测 | P1 | L | 中 | Phase 2 |
| O-B6 | time.After → NewTimer+Stop | 鲁棒性 | P2 | S | 低 | Phase 2 顺手 |
| O-C2 | seccomp-off 分层 fail-closed 校验 | 隔离 | P2 | S | 低 | Phase 1 顺手 |
| O-C3 | uidmap 默认值对齐 | 隔离 | P2 | S | 低 | Phase 1 顺手 |
| O-F12 | 持久化 session/run store + 幂等 | 架构 | P2 | L | 高 | Phase 4 |
| O-F14 | 命令 argv 数组化 | 配置 | P2 | S | 低 | Phase 3 |
| D1 | 统一 agent event 类型（D2 前置） | 可维护性 | P3 | M | 低 | Phase 3 |
| D2 | codex/kimi JSON-RPC session 基类 + 收敛行为漂移 | 可维护性 | P3 | L | 中 | Phase 3 |
| D3/U4 | adapter 共享 helper 提取（仅契约相同部分） | 可维护性 | P3 | M | 低 | Phase 3 |
| U2 | 死代码/死配置清理（保留 constant-time 校验语义） | 可维护性 | P3 | S | 低 | Phase 3 |
| U3 | modelCatalog 描述缓存 | 性能 | P3 | S | 低 | Phase 1（随 O-F04） |
| O-C4 | /etc 挂载收窄 | 隔离 | P3 | S | 低 | Phase 1 顺手 |
| O-F08 | typed 配置直达 adapter（去 env 往返） | 可维护性 | P3 | M | 中 | Phase 3 |

---

## 4. 统一执行路线（最终版）

原则：**每个 Phase 独立可发布；安全/正确性修复不依赖后续阶段；每项独立可合并。**

```
Phase 0 — 正确性与信任边界闭环（对应 ROADMAP Phase 0 + AUDIT Tranche A/B 精选）
  1. U1    session 记录 TTL + pruneLoop + ExecutionHandle.Done() 终态清理
  2. O-A2  JSON-RPC readLoop 解耦（内部有界队列 256 + 单 drain goroutine）
  3. O-F03 native resume ID server-owned（剥离 6 个保留 metadata key）
  4. O-F09a timeout 配置止血（kimi-only 选项改名，或全 adapter 由 worker 边界统一执行）
  5. 顺手：O-B1 / O-B2 / O-B3（三个 S 级低风险项）
  出口：go test ./... + -race 全绿；千级 session 创建-idle-过期压测无单调增长；
        客户端 metadata 无法注入/覆盖 native session ID（含负向测试）。

Phase 1 — 发布与沙箱声明可证明（对应 ROADMAP Phase 1 + AUDIT A3/C 类）
  6. O-C3  uidmap 默认值对齐（1000→65532，O-A3 smoke 的前置）
  7. O-F04 Agent 可用性探针 + /v1/models 只广告可用项 + 镜像 CLI 契约
         （基础镜像不含 CLI、agent 默认禁用；产品镜像 docker/Dockerfile.agents）
         —— **口径已降级（2026-08-14 批准）**：Phase1 可用性 = 命令存在且可执行
         （`exec.LookPath` + 超时），版本/健康/readiness 探针推迟到 Phase5 §5.1。
  8. O-F07 HTTP 加固（ReadHeaderTimeout/IdleTimeout/MaxHeaderBytes +
         MaxBytesReader→413 + 拒绝多 JSON/尾随字节 + metadata 键/长/值上限）
  9. O-C2  seccomp-off 分层 fail-closed（supervisor 层 + nsjail.Build）
 10. O-C4  /etc 挂载收窄（最小文件集：resolv/hosts/nsswitch/passwd/group/ssl）
 11. O-A3  clone_newpid 可配置默认 true + namespaced /proc 挂载（翻转前置，
         非"配套"）+ smoke 加"杀 nsjail 整树死亡"断言
 12. O-F05 非特权（65532/cap-drop=ALL/no-new-privileges/只读 rootfs）CI 安全矩阵
 13. O-F06 seccomp 架构感知（amd64→x86_64、arm64→aarch64、未知架构 fail-closed）
         + arm64 真机 smoke job（cross-build 不足）
 出口：/v1/models 不广告缺失/不可用命令；镜像契约点名 CLI 版本；非特权 Linux
       容器测试通过；每个声称的 arch 跑真 nsjail smoke；slow-header/oversized/
       尾随 JSON/metadata 超限测试通过；文档与 CI 同一安全上下文（ROADMAP §6）。

Phase 2 — Run 治理、观测与资源上限（对应 ROADMAP Phase 2 + AUDIT B 剩余）
 12. O-F11 RunRecord + X-Gateway-Run-Id + 指标/trace/日志规范
 13. O-F10 准入控制 + 配额 + 429 + 日志/workspace 上限
 14. O-B4 codex Abort 兜底；O-B5 清理顺序；O-B6 timer 修复

Phase 3 — typed 配置与去重（对应 ROADMAP Phase 3 + AUDIT Tranche D）
 15. O-F08 typed 配置直达 adapter factory（去 CC_GATEWAY_* env 往返）
 16. O-F14 命令 argv 化（不经 shell，旧字符串形式一个版本 deprecate）
 17. O-F09b 统一 turn deadline 契约（HTTP/run/native 三层 + 流式非流式一致）
 18. D1 → D2（基类抽取并强制收敛 abort/close/usage 漂移）
 19. D3 adapter helper 提取（adapters/internal/bridge，先补 table-driven parity 测试）
 20. U2 死代码清理

Phase 4 — 持久化与幂等（ROADMAP Phase 4 原样保留）
Phase 5 — 远程执行与横向扩展（ROADMAP Phase 5 原样保留）
```

依赖关系（显式标注外互不阻塞）：D2 ← D1；O-B5 消解 ← O-A3；Phase 5 ← Phase 4。

---

## 5. 待决策项（合并去重后）

1. **O-A3 默认开关**：PID namespace 默认开启、以 Linux-CI smoke 为门、保留 `clone_newpid` 开关回退 —— 是否同意？（darwin 开发机无法验证）
2. **U1 record_ttl 默认值**：168h（ROADMAP 示例）vs 24h（AUDIT 建议）。两者都满足 > idle_timeout(2h)；建议 168h，或按业务最长会话间隔定。
3. **O-F09a 止血形态**：统一由 worker 边界执行 turn timeout（首选）vs kimi-only 改名 + 其余拒绝（备选）。实施计划时二选一，不允许 silent。
4. **D2 时机**：L 级、触及 codex/kimi 核心，回报是收敛已漂移行为。Phase 3 立即做 vs 推迟到新 Agent 接入时。
5. **执行粒度**：按 Phase 顺序逐 PR 推进 vs 先挑低风险高回报子集（U1 + O-A2 + O-B1/B2/B3 + O-F09a）。
6. **（ROADMAP 原有，仍有效）** Phase 4 选型 PostgreSQL vs Redis、Phase 5 workspace 策略，届时单独设计评审。
7. **（已决 2026-08-14）O-F04 探针口径**：Phase 1 验收口径降级为「命令存在且可执行（`exec.LookPath`，带超时）」。版本/健康/readiness 探针正式推迟到 Phase 5 §5.1（worker 广告 native CLI 版本）。依据：不变量 #2「API 进程永不启动 Agent CLI」——版本探针需真实 `exec` CLI，必须放 worker 侧。

---

## 6. 每阶段通用门禁（沿用 ROADMAP §11）

- `go test ./...` / `-race` / `go vet` / 三平台 build / `go test ./integration/ -v` / `git diff --check`
- API/runtime/worker 变更必须带集成回归；并发变更必须 race + 压测；sandbox/profile 变更必须真实 Linux jail 执行；安全修复必须带负向测试；每个 bug fix 必须带"修复前会失败"的回归测试。
- 配置变更：safe 时保留默认值、strict TOML 解码、旧 key 迁移/拒绝测试。
- protobuf 只经 `make generate`；日志与错误响应扫描 secret 和宿主路径；每个 phase 同步更新 README/配置示例/架构文档。

---

## 7. 源文档存档位

- `docs/optimization-audit-2026-08-13.md` —— 证据与 `file:line` 出处（保留，作为实施时的查证依据）
- `docs/superpowers/plans/2026-08-13-agent-gateway-optimization-roadmap.md` —— 阶段契约、产品不变量（§1 的 9 条 non-negotiable invariants 全部沿用）、Phase 4/5 详细设计（保留，本文档未重复展开的部分以它为准）
