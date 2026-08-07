//go:build !no_codex

// Registers the Codex adapter into the process-wide runtime registry via its
// init().
package main

import _ "github.com/tianlinzz/agent-cli-gateway/adapters/codex"
