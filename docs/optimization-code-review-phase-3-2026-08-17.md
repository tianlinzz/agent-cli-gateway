# Optimization Phase 3 Code Review (2026-08-17)

## 结论

**不通过。候选 `dd81472898072c5deec5c1914f5b85bd6eac20b0` 暂不允许作为 Phase 3 完成态合并或进入 Phase 4。**

审查区间：

- 基线：`490e5ef4` (`phase-2-review-fixes`)
- 候选：`dd814728` (`phase-3-typed-config-dedup`)
- 范围：`O-F08`、`O-F14`、`O-F09b`、`D1`、`D2`、`D3`、`U2`

阶段 3 的主体结构已经落地：typed adapter config、argv 配置、共享 native event、adapter bridge、JSON-RPC session core、deadline 表面协议和低风险清理均有对应实现与测试；全量质量门禁也通过。但 deadline wrapper 仍会制造跨 turn 的陈旧终态，并能把取消失败误记为已确认 timeout；旧 command 字符串的迁移语义也与路线图明确要求相反。以上均属于进入下一阶段前必须修复的行为/契约问题。

## Findings

### [P1] `native.Send` 失败后 deadline watch 未撤销，会把陈旧 timeout 注入后续 turn

位置：`adapters/internal/bridge/bridge.go:134-140`、`:147-170`、`:204-250`

`Session.Send` 在调用 `native.Send` **之前**先执行 `beginTurn()`，随后直接返回 native 的错误。错误路径没有清掉 `s.turn`、停止 timer 或关闭 `turnWatch.done`。

因此，一个根本没有成功开始的 turn 在 API 已返回 send failure 后仍会继续计时。到期后 watcher 会调用 `native.Abort`；如果 native 当时没有 active turn，`Abort` 通常返回 nil，但不会产生 terminal，watcher 再等待 settle bound，最终把 `runtime.TurnDeadlineExceeded` 写入共享 session event channel。后续请求复用该 handle 时，会先读到这个属于上一次失败请求的陈旧 sentinel，把新 turn 错误呈现为 504/`outcome_unknown`。

这也会在失败请求后无故触发一次原生 Abort，破坏 persistent session 的 turn 边界。

必须补回归测试：

- fake native 的 `Send` 返回错误，超过 turn timeout + settle bound 后不得调用 `Abort`、不得产生任何 terminal/event；
- 同一 wrapper 随后的成功 turn 不得消费到前一请求的 timeout sentinel；
- `beginTurn` 只能在 native 接受 turn 后生效，或 Send 失败必须原子撤销 watch。

### [P1] 原生 Abort 返回失败时，只要 terminal 到达仍被记为已确认 `RunTimedOut`

位置：`adapters/internal/bridge/bridge.go:181-200`、`:204-250`

deadline watcher 调用 `s.native.Abort` 后，把返回值存入 `abortErr`；但只要 terminal 先关闭 `t.done`，watcher 就立即返回，完全忽略 `abortErr`。与此同时 `terminalEvent` 只检查 `t.fired`，无条件把该 native terminal 改写成 `finish(timeout)`。

这意味着 `Abort`/interrupt 已失败或已升级为强杀时，只要 native 侧同时发出 error/cancelled terminal，Gateway 就会把未知完成结果标记为“已确认 timeout”。这与 Phase 2 已确立的终态约束冲突：取消无法确认时必须是 `RunOutcomeUnknown`，不能是 `RunTimedOut`。

现有测试只覆盖：Abort 成功并产生 terminal，以及 Abort 后没有 terminal；没有覆盖“Abort 返回 error + terminal 到达”的关键组合。

必须补回归测试并收敛状态机：

- `Abort` 返回 error，即使 terminal 到达也不能合成 settled `finish(timeout)`；
- 取消失败必须发出 `runtime.TurnDeadlineExceeded`（或等价的显式 unknown-outcome terminal），并确保 execution 被关闭、不可复用；
- 增加真实 API -> Supervisor -> Worker -> bridge 集成用例，避免 API safety-net timer 与 worker timer 的并发 Abort 被 fake backend 测试掩盖。

### [P1] 旧字符串 `command` 仍按 shell-like 语法分词，违反 O-F14 迁移契约

位置：`config/command.go:14-18`、`:37-79`、`:99-159`，`config/command_test.go:42-61`、`:88-123`

路线图 Phase 3.3 明确要求：迁移期若接受旧字符串形式，应发出 deprecation warning，并把它解释为**一个 executable path**，而不是 shell syntax。

当前实现却新增了自定义 shell-like tokenizer，支持空白分词、单双引号 grouping 和反斜杠 escape；测试和 CHANGELOG 还把该行为固化为预期。这样旧字符串继续承担“命令行语言”的角色，保留了本阶段本应消除的解析歧义；例如 `command = "codex --flag"` 会静默变为两个 argv，而不是一个待迁移的 executable path。

应按既定契约处理：

- argv array 是唯一可表达参数的形式；
- deprecated string 整体成为 `argv[0]`，同时告警；
- 删除 shell-like splitter 及其语法测试，增加带空格 executable path 的单 token 回归；
- README/CHANGELOG 与实现保持一致。

### [P2] Phase 3 要求清理的 cc-connect 活跃文档仍在误导贡献者

位置：`CONTRIBUTING.md:1-111`、`CHANGELOG.md:61` 及其后续旧内容

Phase 3.5 明确要求审计并归档/重写 cc-connect 遗留文档。当前 `CONTRIBUTING.md` 仍以 “Contributing to cc-connect” 为标题，指向上游 issue、PR、release、Discord、Telegram 和 WeChat；`CHANGELOG.md` 在新的 Gateway 条目之后继续保留第二个 `Unreleased`，并大篇幅宣称 Feishu/Telegram/Discord、Reasonix 等已被 rewrite 边界删除的能力。

这不是纯历史注释：它们仍位于仓库根目录并作为活跃贡献/发布文档出现，会让贡献者向错误仓库提 issue、按错误发布节奏工作，也与 README/AGENTS 的 clean rewrite 定义冲突。

应重写 `CONTRIBUTING.md` 为 Agent Gateway 流程；旧 changelog 移入明确标注的 archive/provenance 文档，根 `CHANGELOG.md` 只保留当前 Gateway 历史。`TestActiveDocumentation` 也应覆盖 `CONTRIBUTING.md` 和根 changelog 的 IM-era 关键字，而不只检查 README/AGENTS/CLAUDE。

## 分项验收

| 工作项 | 结果 | 说明 |
|---|---|---|
| O-F08 typed config 直达 factory | 基本通过 | `runtime.AdapterConfig` 与 configured registry 已落地，`CC_GATEWAY_*` round-trip 有 archtest 防回归；provider credential/env 的安全边界仍建议在修复轮次中明确记录。 |
| O-F14 command argv | 不通过 | 数组端到端成立，但 legacy string 迁移语义违反 Phase 3.3。 |
| O-F09b turn deadline | 不通过 | HTTP/SSE 表面统一，但 worker wrapper 存在陈旧 terminal 和 settled/unknown 误判。 |
| D1 native event contract | 通过 | 三 agent 使用 `agent/events` alias，依赖方向有 archtest。 |
| D2 JSON-RPC session core | 基本通过 | Core 抽取与 Codex/Kimi 行为收敛完成；本次阻塞主要位于 bridge deadline 状态机。 |
| D3 adapter helper | 基本通过 | 共享映射收敛到 internal bridge，native decode/permission/usage 仍留在 agent 包；缺少路线图要求的三 adapter parity fixture 测试。 |
| U2 dead code/config/docs | 不通过 | 代码/配置清理完成且 constant-time auth 保留，但根 CONTRIBUTING/CHANGELOG 未完成 Phase 3.5 收口。 |

## 新鲜验证证据

以下命令均在候选 `dd814728` 上于 2026-08-17 重新执行并退出 0：

```text
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
go test ./integration/ -v -count=1
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
git diff --check 490e5ef4..dd814728
```

本机无法替代 Linux CI 的真实 nsjail smoke；本阶段没有修改 sandbox profile，因此它不是本次新增 blocker，但发布门仍需沿用 CI 结果。

## 修复后复审入口

复审应冻结新的精确 candidate，并至少重新验证：

1. Send failure 不留下 timer、Abort 或陈旧 event；随后复用 session 的 turn 正常完成。
2. Abort error + terminal 的组合稳定映射为 unknown outcome，且 execution 不可继续复用。
3. 通过真实 Supervisor/Worker 路径覆盖一次 deadline，证明 API 与 worker 双层 timer 不会重复取消或误判终态。
4. legacy string 是单 executable token；只有 argv array 能携带参数。
5. `CONTRIBUTING.md` / 根 `CHANGELOG.md` 不再把 cc-connect IM 能力作为当前项目行为。
6. 重新执行全部通用门禁并报告精确退出状态。
