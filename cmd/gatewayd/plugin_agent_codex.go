//go:build !no_codex

// Registers the Codex adapter into the process-wide runtime registry via its
// init(). This is the NEW gatewayd binary plugin (the legacy cmd/gateway binary
// keeps its own cmd/gateway/plugin_agent_codex.go until task 7 deletes it).
package main

import _ "github.com/tianlinzz/agent-cli-gateway/adapters/codex"
