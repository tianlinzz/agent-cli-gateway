# Phase 1 终审报告（2026-08-14）

审查候选：`90c53069d9aa5f92bd7a6059d255e22903e2237e`，基线为 `6209f82d`。

范围：上一轮 Phase 1 CR 的全部阻断项，以及 O-C3、O-F04/U3、O-F07、O-C2、O-C4、O-A3、O-F05、O-F06 出口条款。

## 结论

**终审：代码修复基本到位，但 Phase 1 仍不能签署“验收通过/发布放行”。**

原因不是 Go 质量门禁，而是：

1. O-F04 的实现仍不是验收要求的版本/健康 readiness probe；
2. 当前没有该候选提交对应的真实 Linux amd64/arm64 smoke 运行 artifact。根据验收文档，真实 Linux/arm64 证据缺失时只能判“无法验收”，不能由代码审查或交叉编译推导通过。

## Findings

### [P1] O-F04 仍只有可执行文件探测，不是版本/健康 readiness probe

`api/openai/models.go:178-201` 仍只对命令首 token 调用 `exec.LookPath`，`compute` 虽新增了 5 秒 `Resolve/Describe` context timeout，但没有：

- 执行 CLI 版本或健康探针；
- 校验版本兼容性；
- 验证命令能实际启动并完成最小握手；
- 将 Agent readiness 与 `/health/ready` 的 nsjail readiness 统一；
- 对坏版本和探针超时提供结构化、不泄密诊断。

现有测试只覆盖 PATH 缺失、空 command 和 `Describe` 失败，仍没有真实 CLI/stub 的缺失、不可执行、坏版本、启动失败和探针超时组合。若项目有意把 O-F04 的“探针”降级为 LookPath + Describe，必须先更新验收口径；按当前验收文档，该项仍为 P1 未完成。

### [P1 / 无法验收] 真实 Linux sandbox artifact 仍缺失

修复后的 `docker/nsjail-smoke.sh` 已改为：

- 通过 `gateway -print-nsjail-profile` 生成实际 `worker/nsjail.Build` profile；
- 检查 seccomp policy、`clone_newpid` 和 namespaced `/proc`；
- 使用真实 `sleep` 并确认 wrapper/子进程确实启动后再 kill；
- 检查整树是否残留。

这解决了上一轮 smoke 脚本的静态缺陷。但当前 `gh run list` 没有返回提交 `90c53069` 对应的 amd64/arm64 CI 运行记录，当前环境也无法执行 Linux nsjail：已有 arm64 镜像在 `--user 65532 --cap-drop=ALL --security-opt=no-new-privileges --read-only` tuple 下返回 `CLONE_NEWUSER|CLONE_NEWPID: Operation not permitted`。新候选镜像的本地构建因 Docker/apt 构建过程未完成而中止，未形成替代证据。

因此 O-F05、O-A3、O-F06、O-C2、O-C3、O-C4 的真实运行部分仍不能签署通过。

## 已确认修复

- F1：smoke 改用真实生成 profile，包含架构选择的 seccomp policy。
- F2：移除不存在的 `sleep2891`，增加 wrapper 存活和 sleeper 已启动断言。
- F3：明确基础产品镜像只交付 Codex + Claude Code；Kimi 改为部署方 derived image，不再虚假传递 `KIMI_VERSION`。
- F4：model catalog 的 Resolve/Describe 重新计算增加 5 秒 context timeout。
- F6：`clone_newpid=false` 在 config 和 Supervisor 两层对 prod/dev fail-closed，仅 test mode 可关闭。

## 门禁结果

- `go test ./... -count=1`：通过。
- `go test -race ./... -count=1`：通过。
- `go vet ./...`：通过。
- `go test ./integration/ -v -count=1`：通过；使用 test mode/stub worker，不是 nsjail 真实运行。
- `GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...`：通过，仅为交叉编译。
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`：通过。
- `go run ./cmd/gateway -print-nsjail-profile smoke`：通过；本机输出 `POLICY aarch64`、`clone_newpid: true` 和 `/proc` mount。
- `git diff --check 6209f82d..HEAD`：通过。

## 最终放行条件

1. 补充实际 Agent CLI/stub 的版本/健康 probe、超时、坏版本测试，并明确 readiness 失败策略；或经批准修改 O-F04 验收口径。
2. 推送候选并保留 amd64、arm64 两套非特权 Linux CI artifact：启动参数、uid/gid、capabilities、mount namespace、真实 profile、seccomp 允许/拒绝结果、PID tree kill 和 worker/stub 运行结果。
3. artifact 通过后，才能把 Phase 1 从“代码修复完成/等待验收”升级为“验收通过”。

