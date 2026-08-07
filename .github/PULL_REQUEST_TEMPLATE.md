<!--
Thanks for contributing! Please fill in the sections below.
The reviewer will use the checklist at the bottom to gate merge.
-->

## Summary

<!-- 1-3 sentences explaining WHAT this PR does and WHY. -->

## Type of change

<!-- Mark all that apply: -->

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that would cause existing behavior to change)
- [ ] Documentation only
- [ ] Internal refactor / chore (no user-visible change)

## Testing

<!-- Explain how you verified the change. Be specific. -->

### Automated tests added in this PR

<!-- List the test functions you added or modified. -->

- `TestX_Y_Z` in `path/to/file_test.go` — what it asserts

### For bug fixes only — regression test

<!-- A bug fix PR MUST include a regression test that:
     (1) fails on the pre-fix code, and
     (2) passes on the fixed code.
     Name it so the bug is searchable later. -->

- Regression test name: `Test...`
- Manual verification this test catches the regression:
  - [ ] Reverted the fix locally; the regression test failed as expected.

### Integration (E2E) impact

<!-- See AGENTS.md → "Testing". The hermetic E2E suite
     (integration/agent_gateway_test.go) drives the real HTTP API against the
     real supervisor and a stub worker child. If your change touches the API
     layer (api/openai/), the runtime contract (runtime/), the worker
     supervisor (worker/), or workspace resolution (workspace/), confirm: -->

- [ ] No integration path touched (small refactor, doc change, etc.)
- [ ] `go test ./integration/ -v` passes locally.
- [ ] If the change alters a user-visible flow, an integration case was added
      (or updated) to cover the new behavior.

## Manual / user-visible behavior change

<!-- Describe what a user will see or do differently after this PR.
     If none, write "None". -->

## Checklist (reviewer will verify)

- [ ] `go build ./...` passes
- [ ] Cross-platform build passes: `GOOS=linux go build ./...` and
      `GOOS=windows go build ./...`
- [ ] `go test ./...` passes (with `go test -race ./...` if touching
      concurrency)
- [ ] `go test ./integration/ -v` passes (for api/runtime/worker/workspace
      changes)
- [ ] AGENTS.md Pre-Commit Checklist items are satisfied
- [ ] No adapter imports in `api/openai/`; `runtime/` stays name-agnostic
      (no hardcoded agent names)
- [ ] No secrets / credentials in source

## Related

<!-- Link to issue, RFC, design doc, prior PR, etc. -->

- Issue:
- Related PR:
