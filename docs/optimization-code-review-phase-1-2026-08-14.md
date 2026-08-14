# Phase 1 代码审查报告（2026-08-14）

审查范围：`a6a45158..6209f82d`，对应合并优化计划中的 Phase 1：O-C3、O-F04/U3、O-F07、O-C2、O-C4、O-A3、O-F05、O-F06。

审查方式：只读审查实现、测试、Docker/CI 配置，并在当前环境运行 Go 门禁及现有 arm64 Docker 镜像上的 smoke 脚本。未修改实现代码。

## 结论

**Phase 1：不通过，暂不可进入验收通过或发布结论。**

Go 代码质量门禁通过，但 Phase 1 的出口条件要求真实 Linux、非特权 nsjail、PID namespace、seccomp 和实际 CLI/stub 组合证据；当前 CI smoke 存在空跑/漏测，且产品镜像没有交付 Kimi CLI，不能将文档声明视为已证明。

## Findings

### [P1] O-F06 smoke 没有加载 seccomp policy，amd64/arm64 job 实际未验证 seccomp

`docker/nsjail-smoke.sh:29-47` 和 `:56-71` 的两个 nsjail 配置都没有 `seccomp_string`，也没有调用 Gateway 生成的 profile。CI 只运行 `/bin/true` 和后续 shell 命令，因此 `.github/workflows/ci.yml:192-215` 的 arm64 job 即使成功，也只能证明无 seccomp 的 namespace jail 能启动，不能证明 `aarch64` Kafel policy 可解析、允许项可运行、拒绝项会拒绝。

验收条款要求交叉编译不能替代目标架构真实 jail + Worker + CLI/stub smoke，且 policy 拒绝项和允许项都必须有运行测试。当前只有 `worker/nsjail/profile_test.go` 的字符串断言，没有运行证据。

### [P1] O-A3 PID 整树 kill smoke 是空跑，无法证明 wrapper 被杀后子进程消失

`docker/nsjail-smoke.sh:73` 启动的是 `sleep2891`，该命令不是镜像中的可执行文件；nsjail 子进程会在测试等待前退出。脚本没有检查 wrapper 是否成功保持运行，也没有确认目标 PID 曾经出现，随后 `:78-84` 仅在 `pgrep` 找到匹配时失败，因此“子进程不存在”会在根本没有创建子进程的情况下通过。

在当前 arm64 Docker 环境用已有 nsjail 镜像执行同一脚本，最小 jail 即返回 `clone(...CLONE_NEWUSER|CLONE_NEWPID) failed: Operation not permitted`；该结果进一步说明必须保留真实 Linux CI 输出，不能只提交脚本和注释。

### [P1] O-F04 产品镜像没有交付 Kimi CLI，三 Agent 镜像契约未闭环

`docker/Dockerfile.agents:43-48` 只安装 Codex 和 Claude Code；Kimi 的安装行仍是注释形式的 `<kimi-cli-package>` TODO。`Makefile:79-84` 虽然传递 `KIMI_VERSION`，但该参数永远不会产生 Kimi 可执行文件。README 将产品镜像描述为可安装三类 CLI，但实际构建产物最多包含两个。

这直接违反“镜像内 CLI 版本/路径与配置契约一致”和“实际镜像/CLI stub 组合验证存在、缺失、坏版本、探针超时”的 O-F04 出口要求。若当前发布明确只交付两 Agent，必须在配置、README 和验收范围中同步降级声明；不能继续声称三 Agent 产品镜像已完成。

### [P1] Agent 可用性探针不是 readiness probe，缺少版本、超时和一致的失败语义

`api/openai/models.go:170-194` 仅对配置命令的首个 `strings.Fields` token 调用 `exec.LookPath`。它没有执行版本/健康探针，没有超时，没有验证版本支持，也没有验证命令实际可启动；`compute` 使用 `context.Background()`（`:147-152`），无法被请求取消或限制探针耗时。`/health/ready`（`api/openai/types.go:510-527`）只检查 nsjail preflight，不检查 Agent catalog/readiness，因此 `/v1/models` 隐藏的不可用 Agent 与 readiness 的失败语义不一致。

同时，验收要求的“坏版本、探针超时、实际镜像/CLI stub”测试不存在。当前测试只覆盖 `LookPath` 缺失和 `Describe` 返回错误，不能证明 O-F04 完整契约。

### [P1] 非特权生产容器的声明没有被实际 Gateway 启动路径验证

CI 只以 `--entrypoint /bin/sh` 运行 smoke（`.github/workflows/ci.yml:181-190`、`:206-215`），从未以同一 `65532 + cap-drop=ALL + no-new-privileges + read-only` tuple 启动 entrypoint、gateway、gateway-worker 并创建 workspace、agent-home、socket/profile 挂载。`docker/entrypoint.sh:25-54` 在没有预挂载 `/srv/workspaces`、`/gateway-run` 和 `/etc/gateway` 时会尝试在只读根文件系统中创建目录/配置；该路径未被 CI 覆盖。

因此 O-F05 的“workspace、agent home、tmp、socket 挂载权限正确”和“完整最小 jail”仍无证据。当前 smoke 只覆盖了 `/tmp` 及部分系统目录，并没有实际 worker/CLI/stub。

### [P1] `clone_newpid` 在 prod/dev 可被普通配置关闭，与其声明的安全边界冲突

`config/gateway.go` 将 `CloneNewPID` 设计为普通布尔配置，`worker/nsjail/profile.go:103-109` 在 prod/dev 也会输出 `clone_newpid: false`。README/代码将 PID namespace 描述为阻止跨 session 信号和保证整树回收的安全边界；而验收条款要求确认其必要时 prod/dev 不得通过普通配置静默关闭。当前实现没有对 prod/dev 的关闭做拒绝或显式高风险开关审计。

如果项目最终批准“prod/dev 可回退关闭”，应修改验收/安全决策并明确该模式不再提供跨 session kill 与整树回收保证；否则应在配置校验层 fail-closed，仅允许 test mode 关闭。

## 按条款状态

| 条款 | 状态 | 说明 |
|---|---|---|
| O-C3 uidmap | 代码部分通过 | 默认值已改为 65532；尚无真实 Linux uidmap 输出证据 |
| O-F04/U3 | 不通过 | 只有 LookPath；无版本/超时/实际 CLI probe；Kimi 未进入产品镜像 |
| O-F07 | 代码部分通过 | listener/body/尾随 JSON 代码已实现；缺真实 slow-header、SSE、health/abort listener 回归证据 |
| O-C2 | 代码通过，运行未验收 | config/supervisor 双层校验存在；真实 seccomp jail 未跑通 |
| O-C4 | 代码部分通过 | `/etc` 已收窄；真实 CLI 所需文件集未验证 |
| O-A3 | 不通过 | PID smoke 空跑；跨 session kill、Abort escalation、crash/shutdown/SIGKILL 整树矩阵缺真实 Linux 证据 |
| O-F05 | 不通过 | CI 没有完整启动 Gateway/worker；当前环境同 tuple 的 namespace 创建失败 |
| O-F06 | 不通过 | arm64 policy 只做字符串/交叉编译；smoke 未加载 seccomp |

## 已执行门禁

- `go test ./... -count=1`：通过。
- `go test -race ./... -count=1`：通过。
- `go vet ./...`：通过。
- `go test ./integration/ -v -count=1`：通过；这是 test mode/stub worker 集成，不是 nsjail 真实运行。
- `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`：通过，仅证明交叉编译。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`：通过，仅证明交叉编译。
- `git diff --check a6a45158..HEAD`：通过。
- `gh run list` 未返回该 Phase 1 提交对应的远端 CI 运行记录；当前没有可引用的 amd64/arm64 Linux smoke artifact。

## 放行前必须补齐

1. 修复 smoke 的可执行 stub 和失败断言，确保目标子进程确实启动后再执行 wrapper kill，并检查整树、`/proc`、PID 1、跨 session signal negative case。
2. 让 smoke 使用与 `worker/nsjail/profile.go` 相同的 seccomp profile，增加允许/拒绝 syscall 的运行测试，并上传 amd64/arm64 artifact。
3. 决定 Kimi 的正式 CLI 包名和固定版本；产品镜像实际安装并验证三 Agent，或将发布范围和文档明确收窄为两 Agent。
4. 定义并实现有超时的版本/readiness probe，使 `/v1/models` 与 `/health/ready` 的失败语义一致；补坏版本、探针超时、缺 CLI、不可执行 CLI 的真实 stub/镜像测试。
5. 用同一非特权只读容器 tuple 启动 entrypoint + gateway，验证 runtime 目录、workspace、agent-home、socket 和 worker mount 全链路。
6. 对 prod/dev 的 `clone_newpid=false` 做 fail-closed 或完成批准的风险记录并调整验收口径。

