# agent-cli-gateway

A standalone HTTP gateway that bridges coding agents (Claude Code, Pi, OpenCode, Codex, Cursor, Gemini CLI, and more) behind a unified session/event API.

Forked from [cc-connect](https://github.com/chenhg5/cc-connect) — the IM platform layer is stripped, replaced with a clean HTTP/SSE API.

## Quick Start

```bash
# Build
go build -o bin/acg ./cmd/gateway/

# Run (requires `claude` CLI installed and authenticated)
./bin/acg -port 4096 -token YOUR_SECRET_TOKEN
```

## API

| Method | Path | Description |
|---|---|---|
| GET | `/health` | Health check + list available agents |
| POST | `/session` | Create a session `{agent, workDir, model, mode}` |
| POST | `/session/:id/prompt_async` | Send a prompt (non-blocking, returns 204) |
| GET | `/event?session=:id` | SSE event stream for the current turn |
| POST | `/session/:id/abort` | Abort the current turn |
| GET | `/config/providers` | Model discovery |

All endpoints except `/health` require `Authorization: Bearer <token>`.

### SSE Event Types

Each `data:` line is a JSON object:

| `type` | Meaning |
|---|---|
| `text` | Text content delta |
| `thinking` | Reasoning/thinking delta |
| `tool_use` | Tool invocation (name + input) |
| `tool_result` | Tool execution result |
| `result` | Turn complete (includes token usage) |
| `error` | Error occurred |
| `permission_request` | Agent requests permission |

### Example

```bash
# Create session
curl -X POST http://localhost:4096/session \
  -H "Authorization: Bearer TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"agent":"claudecode","workDir":"/workspace/project"}'

# Send prompt
curl -X POST http://localhost:4096/session/sess_123/prompt_async \
  -H "Authorization: Bearer TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"parts":[{"type":"text","text":"Fix the bug in main.go"}]}'

# Stream events
curl -N -H "Authorization: Bearer TOKEN" \
  http://localhost:4096/event?session=sess_123
```

## Supported Agents

Claude Code, Pi, OpenCode, Codex, Cursor, Gemini CLI, Kimi, Copilot, Devin, and more (all cc-connect agent drivers are included). Only agents with their CLI installed will be available at runtime.

## Configuration

| Flag | Default | Description |
|---|---|---|
| `-port` | 4096 | HTTP listen port |
| `-token` | (empty) | Bearer token for API auth (empty = no auth) |
| `-data-dir` | (default) | Agent transcript directory |
| `-cors` | (empty) | Comma-separated CORS origins |
| `-version` | | Print version and exit |

## Architecture

```
HTTP Client (NocoBase adapter, curl, etc.)
        │ HTTP + SSE (Bearer token)
        ▼
agent-cli-gateway Gateway
  ├── server/          HTTP handlers + SSE serialization
  ├── server/session_store.go   Session lifecycle (create/resume/abort)
  ├── config/          Gateway configuration
  └── agent/           Agent drivers (from cc-connect, unchanged)
       ├── claudecode/  Claude Code (claude CLI)
       ├── pi/          Pi agent
       ├── opencode/    OpenCode
       ├── codex/       Codex
       └── ...          Cursor, Gemini, Kimi, etc.
```

The gateway preserves cc-connect's agent driver layer (which spawns CLI processes and parses their stream-json output) but replaces the IM platform layer with a clean HTTP/SSE API. The agent loop runs entirely inside the CLI process — the gateway only forwards prompts and streams events.

## License

This project is forked from cc-connect. See upstream for license details.
