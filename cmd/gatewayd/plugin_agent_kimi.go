//go:build !no_kimi

// Registers the Kimi adapter into the process-wide runtime registry via its
// init(). This is the NEW gatewayd binary plugin (the legacy cmd/gateway
// binary keeps its own cmd/gateway/plugin_agent_kimi.go until task 7 deletes
// it).
package main

import _ "github.com/tianlinzz/agent-cli-gateway/adapters/kimi"
