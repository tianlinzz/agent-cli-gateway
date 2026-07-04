# 多用户会话治理设计 (Multi-User Session Governance)

- **状态**: Draft
- **日期**: 2026-07-04
- **目标读者**: ibrian-connect 贡献者
- **定位声明**: 本设计面向 ibrian-connect 作为**开源通用 coding-agent 网关**的演进。所有抽象必须保持调用方无关——不预设任何特定上游（如 NocoBase/f1-web）的协议约定。

---

## 1. 背景与动机

ibrian-connect 当前是一个**单租户、单副本**的 HTTP/SSE 网关：单一共享 Bearer token、无调用方身份概念、会话无上限、无生命周期回收、关停时子进程孤儿化。

审计（见 §10 附录）确认了以下在多用户并发场景下必然出事的问题：

1. **关停/崩溃子进程孤儿化**: `Server.Shutdown` 只关 HTTP listener，不碰 `SessionStore`；`CloseSession` 方法全仓库零调用者。网关退出后 agent 子进程继续运行，占资源、占文件锁。
2. **会话无上限**: 无 max-sessions、无空闲 TTL、无 reaper。会话和子进程数无界增长。
3. **跨用户访问无隔离**: session id 是唯一边界。任何持 token 者持猜中/已知 id 即可操作他人会话。
4. **无调用方身份**: 网关无法区分"是谁在调"，无法做任何 per-caller 控制。

本设计解决这四项 (P0)，使 ibrian-connect 能够安全支撑多用户并发场景。

## 2. 范围

### In Scope (P0)

1. 调用方身份解析（header 注入，可配置 header 名）
2. 会话 owner 归属与跨调用方隔离
3. per-caller 并发会话上限（LRU 淘汰）
4. 空闲会话 TTL 回收
5. 网关关停时清理所有子进程

### Out of Scope (YAGNI)

- 多 agent 路由/编排/并行择优
- 横向扩缩容、外部会话存储（exec.Cmd 句柄无法序列化，单副本架构）
- SSE replay / Last-Event-ID（调用方已有容错路径）
- API key 加密鉴权（保留共享 Bearer，身份走 header）
- `DELETE /session/:id` 端点（reaper + LRU 已覆盖回收需求）
- workDir 强制按 caller 前缀化（隔离由调用方决定，见 §5）

## 3. 非目标与设计原则

- **调用方无关**: 任何 header 名、TTL 值、淘汰策略都是通用配置项，不写死任何上游协议。
- **复用成熟库**: LRU 用 `github.com/hashicorp/golang-lru/v2`，不自造。
- **resume 语义保持**: 子进程被杀/会话被淘汰后，调用方带原 sessionId 可恢复历史（依赖 agent 自身的 resume 能力，如 Claude Code 的 `--resume` 读 transcript 磁盘文件）。
- **向后兼容**: 现有 `POST /session` / `prompt_async` / `/event` / `/abort` 契约不变。新增行为全部通过配置开关，默认值合理时不破坏现有调用方。

## 4. 调用方身份 (Caller Identity)

### 4.1 Header 名可配置

新增配置项 `UserIDHeader`（默认 `"X-User-Id"`）。任何调用方都可设置自己的身份 header 名；网关不绑定任何上游约定。

```toml
# config 语义（gateway 现为 flag/env 驱动，见 §8 配置）
[gateway]
user_id_header = "X-User-Id"
```

### 4.2 身份模式

新增配置项 `IdentityMode`（默认 `"strict"`）：

| 模式 | 行为 |
|---|---|
| `strict`（默认）| 请求缺失身份 header → **401 Unauthorized**。面向多用户场景。 |
| `anonymous` | 请求缺失身份 header → 归为默认用户 `"anonymous"`。面向单用户自用或灰度过渡。提供身份时仍按提供值归属。 |

`anonymous` 模式由环境变量 `GATEWAY_ANONYMOUS_IDENTITY=1` 启用（等价于 `IdentityMode=anonymous`），便于容器化部署注入。

### 4.3 身份格式校验

身份字符串必须匹配 `^[a-zA-Z0-9_-]{1,64}$`，否则 400 Bad Request。这同时是路径注入防护（§5 中身份可能出现在日志/统计里，虽不进 workDir 路径，仍需规范化）。

### 4.4 中间件实现

新增 `withUserIdentity` 中间件，链在 `withAuth` 之后：

```
withAuth (Bearer 校验，不变)
    → withUserIdentity (解析 header，校验格式，注入 context)
        → handler
```

身份通过 `context.Value` 在请求生命周期内传递，handler 通过 `caller.UserIDFromContext(ctx)` 取用。不污染 header 本身。

## 5. workDir 策略（显式不变）

**决策：workDir 保持现有语义，网关不按 caller 强制前缀化。**

理由：共享工作目录是合理用例（多人协作同一 repo、管理员预置共享项目）。强制按 caller 隔离会剥夺这个能力。隔离与否由调用方决定——调用方想隔离就传带身份前缀的路径，想共享就传公共路径。

网关职责保持现状：
- 锚定到网关进程 cwd 下（`resolveWorkDir` 现有逻辑）
- 拒绝绝对路径、拒绝 `..` 越界（现有逻辑）
- `os.MkdirAll` 按需创建

**连带语义**：两个 caller 传同一 workDir = 共享同一份代码（调用方决定），但他们的 session 仍各自独立（owner 不同、LRU 各算各的、事件流不串）。

## 6. 会话归属与跨调用方隔离 (Session Ownership)

### 6.1 ManagedSession 加字段

```go
type ManagedSession struct {
    ID           string
    Agent        core.Agent
    Session      core.AgentSession
    AgentName    string
    Model        string
    CreatedAt    time.Time
    OwnerID      string    // ← 新增：创建该会话的 caller 身份
    LastActivity time.Time // ← 新增：最近一次 Send 时间，LRU/TTL 用
    InFlight     bool      // ← 新增：当前是否有 turn 在跑（atomic bool），TTL reaper 豁免用
}
```

### 6.2 跨调用方访问拒绝

新增 `GetSessionForOwner(id, ownerID)`，替换现有 handler 里的 `GetSession(id)`：

| handler | 现状 | 改后 |
|---|---|---|
| `HandlePromptAsync` | `GetSession(id)` | `GetSessionForOwner(id, userID)` |
| `HandleEventStream` | `GetSession(id)` | `GetSessionForOwner(id, userID)` |
| `HandleAbort` | `GetSession(id)` | `GetSessionForOwner(id, userID)` |

- owner 匹配 → 返回 session
- 不匹配 → **403 Forbidden**（非 404；调用方可信内网，排障友好优先，不防枚举）
- 不存在 → 404 Not Found（保持现状）

### 6.3 LastActivity 更新

`HandlePromptAsync` 成功 `Send` 后更新 `LastActivity = time.Now()`。`/event` 流读取不更新（避免一个挂着的 SSE 连接永久续命一个空闲会话）。

## 7. 会话淘汰 (Eviction: LRU + TTL)

### 7.1 双层淘汰策略

| 维度 | 触发条件 | 作用 |
|---|---|---|
| **per-caller LRU** | 某 caller 活跃会话数 > `MaxSessionsPerUser` | 防止单 caller 爆炸；踢该 caller 最久没活动的会话 |
| **全局 idle TTL** | 任一会话 `LastActivity` 距今 > `SessionIdleTTL` | 无论容量，空闲超时即回收 |

两者并行，谁先触发谁先淘汰。淘汰时执行 **kill + evict**：调 `Session.Close()`（杀子进程）+ 从存储中移除条目。

### 7.2 配置项

| 配置 | env | 默认 | 含义 |
|---|---|---|---|
| `MaxSessionsPerUser` | `GATEWAY_MAX_SESSIONS_PER_USER` | `5` | 单 caller 最大并发会话；`0` = 不限（禁用 per-caller LRU） |
| `SessionIdleTTL` | `GATEWAY_SESSION_IDLE_TTL` | `2h` | 会话空闲超时；`0` = 不限（禁用 TTL） |

### 7.3 LRU 实现（hashicorp/golang-lru/v2）

每个 caller 维护一个独立的 `lru.Cache[string, *ManagedSession]`，容量 = `MaxSessionsPerUser`。

```go
import "github.com/hashicorp/golang-lru/v2"

type SessionStore struct {
    mu              sync.RWMutex
    byID            map[string]*ManagedSession
    perOwner        map[string]*lru.Cache[string, *ManagedSession] // ownerID → LRU
    maxPerUser      int
    idleTTL         time.Duration
    ...
}
```

**LRU 淘汰回调**: `lru.NewWithEvict` 注册 eviction 回调。当 LRU 因容量满而淘汰某条目时，回调被调用。**回调里绝不能直接 `Session.Close()`**——该库在回调期间持有内部锁，而 `Close()` 可能阻塞数秒（gracefulStopTimeout 默认 120s），会死锁。

正确做法（两选一）：

- **方案 A（推荐）: 回调仅标记**。回调只把被淘汰的 `*ManagedSession` 推入一个 `chan *ManagedSession`（容量 buffered，如 256）。独立的 `evictWorker` goroutine 从 channel 取出执行真正的 `Close()` + 从 `byID` map 删。channel 满时（罕见）走 fallback：非阻塞丢弃 + slog.Warn，由下次 reaper 兜底回收。这把"立即淘汰"与"慢速关闭"解耦。
- **方案 B: 回调里 go 闭包**。回调里 `go func(){ defer wg.Done(); ms.Session.Close(); ... }()`。更简单但 goroutine 爆炸风险（大批量淘汰时），且 `byID` 删除竞态要小心。

选 A。`evictWorker` 在 `NewSessionStore` 时启动，`CloseAll` 时通过关闭 channel + `wg.Wait()` 收尾。

**CreateSession 集成 LRU**:
1. 取/建该 caller 的 LRU cache
2. `cache.Add(sessionID, managed)` —— 若导致容量超限，库自动淘汰最久未访问项并触发 eviction 回调
3. 同步写入 `byID` map

**访问即"使用"**: `GetSessionForOwner` 命中时调 `cache.Get(sessionID)`，LRU 库会更新该条目的 recency。

### 7.4 TTL Reaper（全局空闲回收）

后台 goroutine，每 `reaperInterval`（硬编码 1 分钟，或 `SessionIdleTTL/4` 取较小）扫一次 `byID`：

```go
for _, ms := range store.byID {
    if time.Since(ms.LastActivity) > store.idleTTL {
        store.evict(ms) // kill + remove from byID + remove from owner LRU
    }
}
```

reaper 独立于 LRU 容量淘汰——即使 `MaxSessionsPerUser=0`（禁用 LRU），TTL reaper 仍工作。

**运行中 turn 豁免**: reaper 淘汰前检查会话是否有 in-flight turn（`ManagedSession.InFlight bool`，`Send` 时置 true，`result`/`error` 事件回写时置 false）。in-flight 的会话即使超过 TTL 也不淘汰——避免误杀跑了几十分钟的长任务。下次 reaper 周期再判断。`InFlight` 字段加入 §6.1 的 struct 定义。

注意：`/event` 流读取**不**豁免 TTL、**不**更新 `LastActivity`——只有"有 turn 在跑"才算活跃，"挂着个 SSE 连接看历史"不算。

### 7.5 淘汰 = kill + evict（统一函数）

```go
func (s *SessionStore) evict(ms *ManagedSession) {
    // 1. 杀子进程（graceful: SIGTERM → 等 gracefulStopTimeout → SIGKILL）
    if err := ms.Session.Close(); err != nil {
        slog.Warn("evict: close session failed", "id", ms.ID, "error", err)
    }
    // 2. 从 byID 删
    s.mu.Lock()
    delete(s.byID, ms.ID)
    // 3. 从 owner LRU 删（若存在）
    if cache, ok := s.perOwner[ms.OwnerID]; ok {
        cache.Remove(ms.ID)
    }
    s.mu.Unlock()
}
```

**resume 兼容**: 条目从内存删除，但 agent 的 transcript 磁盘历史（如 `~/.claude/projects/`）保留。调用方带原 sessionId 调 `POST /session` → 走新建路径 → agent 层 `--resume <id>` 恢复历史。

## 8. 关停清理 (Graceful Shutdown)

### 8.1 SessionStore.CloseAll

```go
func (s *SessionStore) CloseAll() {
    s.mu.Lock()
    sessions := make([]*ManagedSession, 0, len(s.byID))
    for _, ms := range s.byID {
        sessions = append(sessions, ms)
    }
    s.byID = make(map[string]*ManagedSession) // 清空
    s.perOwner = make(map[string]*lru.Cache[string, *ManagedSession])
    s.mu.Unlock()

    for _, ms := range sessions {
        if err := ms.Session.Close(); err != nil {
            slog.Warn("shutdown: close session failed", "id", ms.ID, "error", err)
        }
    }
}
```

并发关闭（goroutine + WaitGroup）以加速大批量关停，避免串行等待 N 个 gracefulStopTimeout。

### 8.2 Server.Shutdown 集成

```go
func (s *Server) Shutdown(ctx context.Context) error {
    s.store.CloseAll()              // ← 新增：先杀所有子进程
    return s.server.Shutdown(ctx)   // 再关 HTTP
}
```

### 8.3 启动时孤儿清理（可选，P0 bonus）

容器重启后可能残留上一轮孤儿进程。可在网关启动时尝试 `pkill`/taskkill 本机同名 agent 进程。**P0 不强制**——容器场景下 PID 1 退出会带走容器，孤儿问题主要存在于裸机部署。

## 9. 配置汇总

所有新增配置通过 `-flag` / 环境变量注入，与现有 gateway 配置方式一致：

| Flag | Env | 默认 | 说明 |
|---|---|---|---|
| `-user-id-header` | `GATEWAY_USER_ID_HEADER` | `X-User-Id` | 身份 header 名 |
| `-identity-mode` | `GATEWAY_IDENTITY_MODE` / `GATEWAY_ANONYMOUS_IDENTITY=1` | `strict` | `strict`\|`anonymous`；env 兼容两种写法 |
| - | `GATEWAY_MAX_SESSIONS_PER_USER` | `5` | per-caller 最大会话；`0`=不限 |
| - | `GATEWAY_SESSION_IDLE_TTL` | `2h` | 空闲 TTL；`0`=不限 |

`config.GatewayConfig` 扩展为：

```go
type GatewayConfig struct {
    Port              int
    Token             string
    DataDir           string
    CORSOrigins       []string
    // 新增
    UserIDHeader      string
    IdentityMode      string // "strict" | "anonymous"
    MaxSessionsPerUser int
    SessionIdleTTL    time.Duration
}
```

## 10. 测试要求

遵循 AGENTS.md 的测试规范：

1. **回归测试（每个 bug fix 必带）**:
   - `TestShutdown_KillsAllSubprocesses`: 启动 store 加几个 stub session，调 `CloseAll`，断言所有 stub 的 `Close()` 被调用。
   - `TestGetSessionForOwner_RejectsCrossCaller`: owner A 创建，owner B 访问 → 403。
   - `TestCreateSession_StrictMode_RejectsMissingIdentity`: strict 模式无 header → 401。
   - `TestEviction_KillsProcess_AndSupportsResume`: LRU 淘汰后 sessionId 从 map 消失，带同 id 新建走 resume 路径。

2. **并发测试**: `go test -race`，验证多 caller 并发 `CreateSession` + LRU 淘汰无 data race。

3. **CUJ 不直接适用**: 本设计不改 `core/engine.go`/`session.go`（那是 cc-connect 遗留，gateway 不用），但若有人担心回归，可加 gateway 层 CUJ：`TestCUJ_TwoCallers_IsolatedSessions`。

## 11. 风险与权衡

| 风险 | 缓解 |
|---|---|
| LRU eviction 回调里 `Close()` 阻塞死锁 | 回调仅推入 channel，独立 worker 异步关闭（§7.3 方案 A） |
| reaper 误杀正在长时间运行的任务 | **运行中的 turn 豁免 TTL**（§7.4 已规定 reaper 跳过 in-flight 会话） |
| resume 失败（磁盘 transcript 丢失） | 现有 `session_store.go:72-75` 已有 fallback：resume 失败退回新会话 |
| anonymous 模式误用于多用户 | anonymous 设计目标 = 单用户自用/灰度过渡；**不**支撑多用户匿名共用。多用户必须用 strict 模式 + 真实身份。文档与启动日志明确警示 |

## 12. 向后兼容矩阵

| 现有调用方行为 | strict 模式 | anonymous 模式 |
|---|---|---|
| 带 `X-User-Id` header | ✅ 正常，归属该用户 | ✅ 正常，归属该用户 |
| 不带 header | ❌ **401（破坏性）** | ✅ 归为 `anonymous`，正常工作 |

**升级路径**:
- 已部署的调用方不带 header → 默认 strict 模式会 401。升级时需**先设 `GATEWAY_ANONYMOUS_IDENTITY=1`** 过渡，待调用方改造带 header 后切回 strict。
- 单用户自用场景可长期保持 anonymous 模式。

## 13. 待定 (Open Questions)

无。所有 P0 决策已收敛。TTL 豁免运行中 turn 的问题已在 §7.4/§11 解决（reaper 跳过 in-flight 会话）。

实现计划阶段若发现新问题，再开补充设计。

---

## 附: 决策记录

| 决策 | 选择 | 理由 |
|---|---|---|
| 身份机制 | 可配置 header（默认 `X-User-Id`）+ strict/anonymous 模式 | 通用，不绑任何上游协议 |
| workDir | 不强制前缀，调用方决定 | 共享目录是合理用例 |
| 跨用户访问 | 403 | 可信内网，排障优先 |
| LRU 范围 | per-caller | 公平，防跨用户挤占 |
| LRU 库 | hashicorp/golang-lru/v2 | 成熟生产级，不自造 |
| 淘汰后 map | 删条目，磁盘历史保留 | 支持 resume，与 agent 层 --resume 机制契合 |
| per-user 上限默认 | 5 | 经验值，env 可调，0=不限 |
| idle TTL 默认 | 2h | 兜底防泄漏，reaper 主导回收 |
| DELETE 端点 | 不加 | reaper + LRU 已覆盖 |
| API key 加密鉴权 | 不做 | YAGNI，保留共享 Bearer |
