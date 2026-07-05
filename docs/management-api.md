# Management API

The management API exposes **read/write access to the configuration and
sessions of the underlying coding-agent CLIs** (Claude Code, Codex, …) that the
gateway drives. It mirrors what [cc-switch](https://github.com/farion1231/cc-switch)
does as a desktop app — but over HTTP, so you can manage config without
redeploying or hand-editing JSON/TOML files.

All endpoints are mounted on the same gateway port and use the same auth as the
session API. They are safe to call while sessions are running.

> **Scope of v1.** Only the four operations below are managed on-disk. The
> gateway itself never injects this config into a running subprocess — the CLI
> reads it from its own files on the next session start (cc-switch "file
> management only" mode). Provider/MCP changes therefore take effect for the
> **next** `POST /session`, not for an already-running session.

---

## Contents

- [Authentication](#authentication)
- [Conventions](#conventions)
- [Session Management](#session-management)
  - [List sessions](#list-sessions)
  - [Session history](#session-history)
  - [Delete a session](#delete-a-session)
  - [Session resume info](#session-resume-info)
- [Provider Live Config](#provider-live-config)
  - [Read provider](#read-provider)
  - [Write provider](#write-provider)
- [MCP Servers](#mcp-servers)
  - [List MCP servers](#list-mcp-servers)
  - [Save an MCP server](#save-an-mcp-server)
  - [Delete an MCP server](#delete-an-mcp-server)
- [Agent capability matrix](#agent-capability-matrix)
- [Field reference](#field-reference)

---

## Authentication

Same as the session API. If the gateway is started with `-token`, every
management endpoint requires:

```
Authorization: Bearer <token>
```

`X-Api-Key: <token>` is also accepted. Requests are rejected with `401` if the
token does not match. With `-token ""` (dev mode) auth is skipped.

## Conventions

- **Content type:** `application/json` for request and response bodies.
- **Field naming:** `camelCase`, matching cc-switch's `SessionMeta` shape.
- **Timestamps:** Unix seconds (int64).
- **Errors:** `{ "error": "<message>" }` with an appropriate HTTP status.
- **Agent-not-found:** `404 Not Found`.
- **Capability not implemented** (e.g. an agent without `LiveConfigProvider`):
  `501 Not Implemented`.
- **Global config caveat:** Provider and MCP writes edit a shared file
  (`~/.claude/settings.json`, `~/.codex/config.toml`, …). They affect **all**
  callers' subsequent sessions — there is no per-user isolation in v1.

---

## Session Management

These endpoints read the on-disk session transcripts that the underlying CLIs
already maintain (`~/.claude/projects/*/*.jsonl`, `~/.codex/sessions/*.jsonl`).
They do **not** create or touch live gateway sessions.

### List sessions

```
GET /sessions?agent={name}
```

Lists sessions known to the agent backend. `agent` is required.

**Example**

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4096/sessions?agent=claudecode"
```

**200 —** `application/json`

```jsonc
{
  "agent": "claudecode",
  "sessions": [
    {
      "agent": "claudecode",
      "id": "a1b2c3",
      "summary": "fix bug in main.go",
      "messageCount": 12,
      "modifiedAt": 1750000100,
      "gitBranch": "main"
    }
  ]
}
```

**Errors**

| Status | When |
|---|---|
| `400` | missing `agent` query param |
| `404` | unknown agent name |
| `500` | agent backend scan error |

---

### Session history

```
GET /sessions/{agent}/{id}/history?limit=N
```

Returns the user/assistant turns of a session transcript. `limit` is optional;
`0`/missing means "no limit" and returns the last `N` entries when set.

**Example**

```bash
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:4096/sessions/claudecode/a1b2c3/history?limit=20"
```

**200 —**

```jsonc
{
  "agent": "claudecode",
  "id": "a1b2c3",
  "entries": [
    { "role": "user",      "content": "Fix the bug in main.go", "timestamp": 1750000000 },
    { "role": "assistant", "content": "I'll start by reading…",  "timestamp": 1750000010 }
  ]
}
```

**Errors**

| Status | When |
|---|---|
| `404` | agent unknown, or session file not found |
| `501` | agent does not implement history reading |

---

### Delete a session

```
DELETE /sessions/{agent}/{id}
```

Deletes the on-disk transcript file. **This does not abort any live gateway
session** that happens to be using the same id — call
`POST /session/{id}/abort` first if you need to stop a running session.

**Example**

```bash
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://localhost:4096/sessions/claudecode/a1b2c3
```

**200 —** `{ "agent": "claudecode", "id": "a1b2c3", "ok": true }`

**Errors**

| Status | When |
|---|---|
| `404` | session file not found |
| `501` | agent does not implement session deletion |

---

### Session resume info

```
GET /sessions/{agent}/{id}/resume
```

Returns the native CLI resume command for reference, plus a hint describing how
to actually resume through the gateway.

**Example**

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:4096/sessions/claudecode/a1b2c3/resume
```

**200 —**

```jsonc
{
  "agent": "claudecode",
  "id": "a1b2c3",
  "resumeCommand": "claude --resume a1b2c3",
  "gatewayHint": "POST /session with {\"agent\":\"claudecode\",\"sessionId\":\"a1b2c3\"} to resume via gateway"
}
```

`resumeCommand` is empty if the agent does not implement `ResumeCommander`.

---

## Provider Live Config

Read or write the **active provider** of an agent's backing CLI. The "basic
triple" is managed: `apiKey`, `baseUrl`, `model` (plus optional passthrough
`env`).

The exact files touched per agent:

| Agent | File(s) written | Field mapping |
|---|---|---|
| `claudecode` | `~/.claude/settings.json` | `env.ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY`, `env.ANTHROPIC_BASE_URL`, `env.ANTHROPIC_MODEL` |
| `codex` | `~/.codex/auth.json` + `~/.codex/config.toml` | `OPENAI_API_KEY` (auth.json); `model`, `model_provider`, `[model_providers.<id>].base_url` (config.toml) |

Writes are **atomic** (temp file + rename). For Codex, the two files are written
together with rollback: if `config.toml` fails to write after `auth.json` has
been updated, `auth.json` is restored to its previous bytes.

> Writes preserve all other fields in the file (permissions, hooks, OAuth
> tokens, MCP config, …). The first cut does **not** cover cc-switch's advanced
> Codex features (bearer-token elevation, model catalog, `web_search` policy,
> unified session bucket) — see "Scope of v1" above.

### Read provider

```
GET /config/agents/{agent}/provider
```

**Example**

```bash
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:4096/config/agents/claudecode/provider
```

**200 —**

```jsonc
{
  "agent": "claudecode",
  "apiKey": "sk-...",
  "baseUrl": "https://relay.example.com",
  "model": "claude-sonnet-4",
  "env": { "CLAUDE_CODE_USE_BEDROCK": "1" }
}
```

For Claude Code, `apiKey` reports `ANTHROPIC_AUTH_TOKEN` when present (third-party
base URL case), otherwise `ANTHROPIC_API_KEY`.

**Errors**

| Status | When |
|---|---|
| `404` | unknown agent |
| `501` | agent does not expose live provider config |

---

### Write provider

```
PUT /config/agents/{agent}/provider
Content-Type: application/json
```

**Body**

```jsonc
{
  "apiKey":  "sk-...",                       // optional
  "baseUrl": "https://relay.example.com",    // optional
  "model":   "claude-sonnet-4",              // optional
  "env":     { "CLAUDE_CODE_USE_BEDROCK": "1" } // optional
}
```

All fields are optional. The request is treated as a **full write of the basic
triple**: send `""` to explicitly clear a field, omit only if you don't care
about its value. Unrelated `env` keys you pass in `env` are merged in; an empty
value clears that key.

**Example**

```bash
curl -X PUT -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  http://localhost:4096/config/agents/codex/provider \
  -d '{"apiKey":"sk-new","baseUrl":"https://relay.example.com","model":"gpt-5"}'
```

**200 —** echoes back the canonical on-disk state (same shape as the GET
response).

**Errors**

| Status | When |
|---|---|
| `400` | malformed JSON body |
| `404` | unknown agent |
| `500` | I/O failure (disk full, permission denied, …) |
| `501` | agent does not expose live provider config |

---

## MCP Servers

Manage the MCP server definitions the backing CLI loads on its next session.

| Agent | File | Section |
|---|---|---|
| `claudecode` | `~/.claude.json` | `mcpServers` (top-level object) |
| `codex` | `~/.codex/config.toml` | `[mcp_servers.*]` |

**Windows note.** On Windows, stdio servers whose command stem is
`npx` / `npm` / `yarn` / `pnpm` / `node` / `bun` / `deno` are automatically
rewritten to `cmd /c <command> <args…>` so the spawned shell resolves the `.cmd`
shim. Commands that are WSL UNC paths (`\\wsl$\…`, `\\wsl.localhost\…`) are
exempt. (Mirrors cc-switch's `wrap_command_for_windows`.)

### List MCP servers

```
GET /config/agents/{agent}/mcp
```

**200 —**

```jsonc
{
  "agent": "claudecode",
  "servers": {
    "filesystem": {
      "agent": "claudecode",
      "name": "filesystem",
      "type": "stdio",
      "command": "cmd",                       // wrapped on Windows
      "args": ["/c", "npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "env": { "ROOT": "/tmp" }
    },
    "remote": {
      "agent": "claudecode",
      "name": "remote",
      "type": "sse",
      "url": "https://mcp.example.com/sse"
    }
  }
}
```

`type` is empty/`"stdio"` for local command servers; `"sse"`/`"http"` for
remote.

---

### Save an MCP server

```
PUT /config/agents/{agent}/mcp/{name}
Content-Type: application/json
```

Upserts a single MCP server by `name`. Other entries and unrelated config
fields are preserved.

**Body**

```jsonc
// stdio
{
  "command": "npx",
  "args":    ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
  "env":     { "ROOT": "/tmp" }
}

// sse / http
{
  "type": "sse",
  "url":  "https://mcp.example.com/sse"
}
```

**200 —** the saved entry (same shape as one item in the list response).

**Errors**

| Status | When |
|---|---|
| `400` | missing `name` path param, malformed JSON, or validation failure (stdio without `command`; sse/http without `url`) |
| `404` | unknown agent |
| `500` | I/O failure |
| `501` | agent does not expose MCP config |

---

### Delete an MCP server

```
DELETE /config/agents/{agent}/mcp/{name}
```

Idempotent: deleting a name that doesn't exist is `200`, not an error. When the
last server is removed on Codex, the now-empty `[mcp_servers]` section is also
dropped.

**200 —** `{ "agent": "claudecode", "name": "filesystem", "deleted": true }`

---

## Agent capability matrix

Not every agent implements every operation. Capability is detected at runtime;
calling an unsupported one returns `501 Not Implemented`.

| Capability | Interface | claudecode | codex | others |
|---|---|---|---|---|
| List sessions | `core.Agent` (required) | ✅ | ✅ | per agent |
| Read history | `HistoryProvider` | ✅ | ✅ | per agent |
| Delete session | `SessionDeleter` | ✅ | ✅ | per agent |
| Resume info | `ResumeCommander` | ✅ | ✅ | optional |
| Provider live config | `LiveConfigProvider` | ✅ | ✅ | optional |
| MCP config | `McpConfigManager` | ✅ | ✅ | optional |

To add support for another agent (e.g. `gemini`), implement the relevant
interface(s) on that agent type — no changes to `server/` or `core/` are needed.

---

## Field reference

### `LiveProviderConfig`

| Field | Type | Notes |
|---|---|---|
| `apiKey` | string | Claude: `ANTHROPIC_AUTH_TOKEN` (3rd-party) or `ANTHROPIC_API_KEY` (1st-party). Codex: top-level `OPENAI_API_KEY` in auth.json. |
| `baseUrl` | string | Claude: `ANTHROPIC_BASE_URL`. Codex: `[model_providers.<active>].base_url`. |
| `model` | string | Model identifier passed to the CLI. |
| `env` | `map[string,string>` | Extra env passthrough (e.g. `CLAUDE_CODE_USE_BEDROCK=1`). Empty value clears the key. |

### `McpServerConfig`

| Field | Type | Notes |
|---|---|---|
| `type` | string | `"stdio"` (default) / `"sse"` / `"http"` |
| `command` | string | Required for stdio. Windows-wrapped if it's npx/npm/… |
| `args` | string[] | Optional, stdio only |
| `env` | `map[string]string` | Optional, stdio only |
| `url` | string | Required for sse/http |

### `SessionListItem`

| Field | Type | Notes |
|---|---|---|
| `agent` | string | Echoes the queried agent name |
| `id` | string | Session id from the CLI's transcript store |
| `summary` | string | First user prompt, truncated |
| `messageCount` | int | User + assistant turns |
| `modifiedAt` | int64 | Unix seconds, transcript mtime |
| `gitBranch` | string | Optional |

### `HistoryEntryDTO`

| Field | Type | Notes |
|---|---|---|
| `role` | string | `"user"` or `"assistant"` |
| `content` | string | Text content of the turn |
| `timestamp` | int64 | Unix seconds, omitted when unknown |
