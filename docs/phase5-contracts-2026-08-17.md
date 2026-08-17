# Phase 5 契约与本地兼容设计说明

> 日期：2026-08-17
> 范围：ROADMAP §10 Phase 5 的「只做契约与本地兼容」子集 —— Worker 注册/
> 版本协商握手 + `ExecutionBackend` 抽象验证 + workspace 策略记录。
> 显式排除：远程 worker 调度、lease/CAS、节点亲和、drain（§5.2 与 §5.3 的
> 落地留给后续单独设计评审）。
> 依据：`docs/superpowers/plans/2026-08-13-agent-gateway-optimization-roadmap.md`
> §10、`docs/optimization-plan-merged-2026-08-13.md` Phase 5。

## 1. Worker 注册握手

Worker 注册复用既有的 `Worker.Health` 握手 RPC
（`worker/proto/worker.proto`）：supervisor fork 出 per-session worker 后的
第一个 RPC 就是 Health，注册字段随 `HealthResponse` 返回，不引入新的 RPC
或注册通道。

`HealthResponse` 字段语义：

| 字段 | 语义 | 来源 |
| --- | --- | --- |
| `status` | 成功时为 `"ok"` | worker 进程 |
| `version` | worker 实现版本字符串（自由文本，仅观测用） | worker.Handler |
| `pid` | worker 进程自身 PID（pid namespace 共享时即宿主 PID） | `os.Getpid()` |
| `worker_id` | worker 进程身份。一 worker 一 session 模型下等于 gateway session id（`GW_WORKER_SESSION_ID`）；脱离 supervisor 启动时退化为 `worker-<pid>` | worker 进程环境 |
| `node_id` | worker 所在节点身份。单机部署取 `os.Hostname()`；不引入新的 config 项，配置化 node id 留给跨节点阶段 | worker 进程 |
| `protocol_version` | worker 使用的握手协议版本，见 §2 | `worker.ProtocolVersion` 常量 |
| `arch` | worker 二进制的 `runtime.GOARCH` | worker 进程 |
| `sandbox_capabilities` | 生效中的隔离能力。jailed worker 报告 `nsjail`（以 supervisor 仅对 jailed worker 设置的 `GW_AGENT_HOME` 为在押标记）；非隔离（test profile）为空 | worker 进程环境 |
| `adapters` | worker 进程内注册的 adapter 名（来自进程级 runtime registry；worker child 通过可选接口 `worker.AdapterLister` 提供） | worker 进程 |

与 ROADMAP §5.1 的两点显式偏差/细化：

- **不广告 native CLI 版本**。per-session 进程模型下 worker 是 supervisor
  fork 的同机子进程，CLI 可用性/版本由 supervisor 本地探测（
  `exec.LookPath` 口径，Phase 1 已批准）即可，不需要也不应该伪造 worker
  自报；跨节点阶段引入真正远程 worker 时再评审该字段。
- **不广告容量/当前负载、无 lease 字段**。它们属于调度与租约语义
  （§5.2），本轮不做。

protobuf 只经 `make generate` 重新生成，禁止手改 `worker.pb.go` /
`worker_grpc.pb.go`。

## 2. 版本协商规则（fail-closed）

- 单一来源：`worker.ProtocolVersion`（`worker/version.go`）。
  `cmd/gateway`（supervisor 侧）与 `cmd/gateway-worker`（worker 侧）都从同
  一模块 import 该包，同一构建内两侧常量是同一值。
- worker 在 `HealthResponse.protocol_version` 中上报该常量；supervisor 在
  握手时与自身常量比对，**任何不一致即拒绝启动该 session**：worker 进程被
  回收，错误上抛，API 层按既有握手失败路径返回 500 + error_code。不存在
  「降级继续」路径 —— 混版本对必须在启动时响亮失败，不允许漂移到
  session 中途。
- 何时 bump：握手语义的不兼容变更（字段含义、协商规则变化）必须 bump。
  纯新增 proto 字段不需要 bump —— proto3 的未知字段容忍保证旧对端可继
  续工作。
- 心跳（heartbeat）复用 Health 但只做 PID 一致性检查，不做版本再协商：
  版本在进程生命周期内不可变。
- 负向测试：`worker/handshake_test.go`
  `TestHandshake_ProtocolVersionMismatchRejected`（版本不匹配被拒，会话未
  启动）。

## 3. 身份落记录

`runtime.SessionRecord.WorkerID/NodeID`（字段早已预留）在本地路径端到端
填充：

1. supervisor 握手成功后把 `worker_id`/`node_id` 存入 `workerSession`
   （`worker/supervisor.go`）。
2. `workerSession` 实现可选能力 `WorkerIdentity() (workerID, nodeID string)`；
   `runtime.ExecutionHandle` 接口本身不变。
3. API 层在 `backend.Start` 成功后（含死 handle 重建路径）通过可选接口
   断言 + `SessionStore.Update` 把身份写入 session 记录
   （`api/openai/chat_completions.go` `recordWorkerIdentity`）。该写入是
   best-effort 观测元数据：handle 不支持该能力或 store 出错都不会让请求
   失败，字段保持为空。

跨节点阶段这些字段即成为调度和故障关联的事实来源；当前它们证明契约字
段在本地路径可用。

## 4. ExecutionBackend 抽象验证

「远程执行只换 backend」契约的回归保护：

- `api/openai/backend_contract_test.go`：编译期断言 in-process
  `fakeBackend` 满足 `runtime.ExecutionBackend`，并用它驱动完整 HTTP
  非流式 completion —— API 层代码零改动运行在纯内存 backend 上，无
  worker 进程、无 socket、无 gRPC。
- `api/openai` 不 import `worker/`、`adapters/*`、`config/` 由
  `internal/archtest` 强制（既有门禁，未改动）。

## 5. Workspace 策略：共享持久文件系统（本轮仅文档化）

ROADMAP §5.3 要求显式选定一种策略。选定 **shared persistent
filesystem**：所有（当前唯一的）节点挂载同一持久文件系统，`workspace_id`
解析为该文件系统上受控 root 下的目录，worker 通过 bind mount 进入沙箱。
公开契约不变：客户端仍只提交 opaque `workspace_id`，绝不能注入绝对路径。

选定该策略必须如实记录的行为与注意点：

- **一致性**：依赖底层文件系统的一致性语义（本地盘为强一致；若日后挂在
  NFS/对象存储网关类后端上，close-to-open 一致性、元数据缓存会让刚写入
  的文件对其他节点延迟可见 —— 跨节点调度落地前必须验证所选后端的语义，
  不能假设 POSIX 强一致）。
- **锁**：并发规则仍是「每 session 一个活跃 turn」；同一 workspace 的不同
  session 可以并发执行，文件系统协调策略由调用方自负（AGENTS.md 既有口
  径）。Agent CLI 自身的锁文件（如会话 transcript 锁）在共享文件系统上可
  靠的前提是锁实现（flock/O_EXCL）被该后端正确支持。
- **清理**：session 回收不删除 workspace 内容；workspace 目录的生命周期归
  部署方/调用方，gateway 只清理自身的 runtime 目录（socket/profile/log）。
  清理策略（配额、留存期）需要部署侧另行定义。
- **凭据**：workspace 内容对同节点所有 jailed worker 按 mount 可见；凭据
  （agent token、git 凭据）一律走 per-session agent-home 与环境注入，绝不
  写入共享 workspace 目录。多节点后需评审「一个节点的失陷是否经共享文件
  系统扩散」。
- **性能**：每 turn 的 IO 直接落在共享后端上；高 churn 场景需评估后端
  IOPS/延迟，但本轮不做基准。

备选策略（per-run checkout/materialization、caller-managed external
volume）本轮不实现，跨节点设计评审时再议。

## 6. 显式不做（留给后续）

以下属于 ROADMAP §5.2/§10 出口标准，本轮**未实现也未声明满足**：

- 远程 worker 调度与放置（含按 arch/sandbox/adapters 的兼容性选择）；
- session 的节点亲和与跨节点 resume 放置；
- lease/CAS 所有权（防止两节点跑同一 session turn）；
- 节点 drain、节点失联时 active run 的 outcome_unknown 标记链路；
- worker 容量/负载广告与基于负载的调度；
- 配置化 node id（当前固定 `os.Hostname()`）。
