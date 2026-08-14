# Phase 2 代码审查报告（2026-08-14）

审查范围：`2da51704..2fb63ff1`，覆盖 O-B4、O-B5、O-B6、O-F11、O-F10。

## 结论

**Phase 2：不通过。** Go 质量门禁通过，但 admission 和 Run governance 仍存在会导致长期运行错误的实际缺陷，且 Phase 2 出口要求的指标覆盖、session/run cap 和故障压测证据不完整。

## Findings

### [P1] Admission 成功后 BeginTurn 失败会永久泄漏 active-run slot

`api/openai/chat_completions.go:323-341` 成功调用 `h.admit.Acquire` 后，释放 defer 直到 `:389-398` 才建立；但 `BeginTurn` 位于 `:343-362`。当 session busy、closing、not found 或 store error 导致 `BeginTurn` 失败时，函数直接返回，既没有 `h.admit.Release`，也没有 defer 兜底。

结果是 `activeRuns`、callerRuns 和 workspaceRuns 永久偏高，最终所有后续请求被错误地 429。该路径没有回归测试。

### [P1] Global active-run cap 存在并发超卖

`api/openai/admission.go:96-120` 在 mutex 外使用 `activeRuns.Load()` 判断 global cap，进入 mutex 后没有再次检查。两个不同 caller/workspace 并发时都可能读取到相同的未满值，随后依次提交计数，导致 `activeRuns > MaxActiveRuns`。

per-caller/per-workspace 检查在锁内，但 global 检查没有和计数提交形成原子操作；现有测试只有串行 Acquire，没有并发 oversubscription 测试。

### [P1] `max_workers` 超限不会返回 429，而是创建 session 后返回 500

`AdmissionLimits.MaxWorkers` 在 `api/openai/admission.go:72-80` 定义但未在 `Acquire` 中使用。实际 worker 上限只在 `worker/supervisor.go` 的 `StartSession` 中检查；API 已经创建 session、Acquire active-run slot、BeginTurn 后才调用 backend.Start，失败路径 `api/openai/chat_completions.go:416-424` 将其映射成 500 `start_failed`。

这违反 O-F10 “达到资源上限快速、确定地返回 429，且不得先创建 Worker 再拒绝”的过载契约。max_workers 需要在 API admission 层作为可返回 429 的准入条件，或由一个统一的 admission owner 负责。

### [P1] RunStore 没有接入 prune loop，RunRecord 会无限增长

`runtime.RunStore` 提供了 `Prune`，但 `api/openai/types.go:390-411` 的后台 prune loop 只调用 `h.store.Prune`（SessionStore），全仓没有 `h.runs.Prune` 调用。`RunRecord` 在每个 turn 创建，并不会因 `SessionRecordTTL` 或 `PruneInterval` 被删除。

因此 O-F11 的内存态 RunStore 长期运行仍会泄漏；Phase 2 出口要求的 RunRecord 生命周期闭环没有完成。

### [P1] Phase 2 延期的 session record cap 仍不完整

配置只有 `MaxSessionsPerCaller`，没有 global session record cap。`docs/optimization-acceptance-2026-08-13.md:367` 明确要求 Phase 0 延期的 global/per-caller session record cap 在 Phase 2 完成，否则 U1 只能有条件通过。

当前 per-caller 检查还在 `Create` 之后通过 `List` 统计，缺少统一的原子 reservation；global cap 完全不存在。高并发新 session 场景不能作为严格的容量边界。

### [P1] O-F11 指标没有覆盖验收要求的 active/queued、abort、timeout、worker crash、reclaim

全仓实际写入的指标主要是：`gateway_runs_total`、`gateway_run_duration_seconds`、`gateway_unknown_outcomes_total`、`gateway_agent_tokens_total` 和 admission rejection。`api/openai/chat_completions.go:755-764` 没有 active/queued gauge、abort counter、timeout counter、worker crash counter 或 reclaim counter。

这不满足验收条款对 active/queued runs、abort、timeout、worker crash、unknown outcome、reclaim 和 admission reject 的完整覆盖。当前测试只断言少数 Prometheus 文本存在，没有逐终态矩阵。

### [P1] 结构化 run 日志缺少验收要求的 caller/session/workspace/Worker 状态

`api/openai/chat_completions.go:767-775` 的 `run finished` 日志包含 run/request/trace/adapter/status/duration/error_code，但没有 caller、Gateway session、opaque workspace ID 或 Worker 状态。RunRecord 内虽然保存了这些字段，但 `finishRun` 的参数和日志没有传递它们。

因此日志无法单独完成验收要求的端到端关联，也不能区分 worker crash/reclaim 等终态来源。

### [P1] B5 的 reaped 等待有超时逃逸，超时后仍删除运行目录

`worker/supervisor.go:999-1025` 最多等待 2 秒；超时后只记录 warning，随后继续 `RemoveAll(sessionDir/socketDir)`。这仍可能在进程树未 reaped、子进程持有 socket/log 的情况下删除依赖目录，和“先确认进程树退出，再删除目录”的验收要求不一致。

如果必须有上限，应在超时路径执行明确的二次强杀/descendant 确认并将清理标记为 deferred，而不是无条件移除目录。

### [P2] B6 仍残留 time.After，未形成全路径 timer 回收护栏

Phase 2 虽替换了主要 turn/stream timer，但 `api/openai/chat_completions.go:606` 的 abort endpoint、`worker/supervisor.go:391,752,758,1008` 仍使用 `time.After`。这些不一定构成持续泄漏，但与“timer 使用可停止/回收机制”的全路径要求不一致，也没有静态门禁防止回归。

### [P2] worker log cap 是周期性 best-effort，不能保证配置上限

`worker/supervisor.go:870-918` 每 30 秒检查一次并截断。窗口内日志可超过上限；当配置 cap 小于 2048 字节时，`tailSize` 被强制为 1024，再加上 truncation marker，最终文件必然大于配置值。当前没有 log-cap 回归测试，也没有源头有界写入或严格 rotation 契约。

## 条款状态

| 条款 | 状态 | 说明 |
|---|---|---|
| O-B4 | 代码部分通过 | Codex turn-id 等待与受控 teardown 有回归测试；仍需故障压测确认 slot/RunRecord 终态联动 |
| O-B5 | 不通过 | reaped 等待超时后仍删除目录 |
| O-B6 | 代码部分通过 | 主要 timer 已替换，但仍有 time.After 残留 |
| O-F11 | 不通过 | RunStore 不 prune；指标和日志字段不完整 |
| O-F10 | 不通过 | slot 泄漏、global cap 竞态、max_workers 非 429、global session cap 缺失 |

## 已执行门禁

- `go test ./... -count=1`：通过。
- `go vet ./...`：通过。
- `go test -race ./... -count=1`：通过。
- `go test ./integration/ -v -count=1`：通过；主要是 test mode/stub worker，未覆盖真实 admission 压测。
- `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`：通过。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`：通过。
- `git diff --check 2da51704..2fb63ff1`：通过。

## 放行前必须补齐

1. 将 admission reservation/defer 前移到所有可能失败的路径，补 BeginTurn failure slot-release 回归测试。
2. 在锁内原子检查并提交 global cap，补多 caller 并发 oversubscription 测试。
3. 将 MaxWorkers、global session cap 纳入统一 API admission，并确保所有上限走 429 + Retry-After。
4. 将 RunStore.Prune 接入生命周期 loop，完成 global/per-caller session record cap 和故障压测后的回落验证。
5. 补齐 active/queued、abort、timeout、worker crash、reclaim 指标和 caller/session/workspace/worker 状态日志字段。
6. 重新定义 B5 超时清理策略，确保未 reaped 时不无条件删除依赖目录；清理剩余 `time.After` 并加静态门禁。

