# Contributing to agent-cli-gateway

Thank you for using the Agent Gateway and for every issue, pull request, and
piece of feedback that helps improve it.

`agent-cli-gateway` is an OpenAI-compatible Agent Gateway: it exposes coding
agents (Codex, Claude Code, Kimi) behind a single HTTP API (`/v1/models`,
`/v1/chat/completions` with SSE streaming) and forks one
nsjail-isolated worker process per session. It is a clean rewrite of
cc-connect — the IM platform layer is intentionally out of scope here.

## Before You Open An Issue Or PR

1. Search first. Check existing issues and pull requests in this repository
   for duplicates or related discussion before starting new work.
2. Read [`AGENTS.md`](./AGENTS.md) — it is the development guide every change
   is reviewed against (dependency rules, isolation requirements, testing
   requirements, and the pre-commit checklist).

## Writing A Helpful Issue

Please include as much of the following as possible:

- Version: `gateway -version` output
- Environment: OS, runtime mode (`prod` / `dev` / `test`), whether nsjail is
  in use, and which agent CLIs are installed
- Reproduction steps: the smallest API request (curl) that shows the problem
- Expected behavior vs. actual behavior
- Logs or errors, with tokens/secrets redacted
- Optional analysis or a proposed fix

Security-sensitive reports (sandbox escapes, auth bypasses) should avoid
public issues until triaged.

## Pull Requests

- Follow the repo guidance in [`AGENTS.md`](./AGENTS.md) and
  [`CLAUDE.md`](./CLAUDE.md).
- Run the local gates before submitting — at minimum:

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...          # required for concurrency-sensitive changes
go test ./integration/ -v    # required for api/runtime/worker changes
```

- Cross-platform builds must stay green:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
```

- Every bug fix must include a regression test that fails on the pre-fix code
  (name it so the bug is searchable later).
- Call out breaking changes explicitly in the PR description — especially
  config key removals (strict TOML decoding rejects unknown keys) and worker
  RPC/proto changes (gateway and worker deploy as one version).
- Update docs or examples (`README.md`, `config.example.toml`) when behavior
  or configuration changes.
- Never commit tokens, API keys, or credentials; the repo is scanned for
  secrets in review.

## Release Cadence

Releases follow the phased optimization roadmap
(`docs/optimization-plan-merged-2026-08-13.md`); each phase is independently
mergeable and must pass the full gate set, with sandbox claims proven by the
Linux CI (docker/nsjail-smoke.sh) rather than the darwin dev host. Treat the
GitHub Releases page of this repository as the source of truth.
