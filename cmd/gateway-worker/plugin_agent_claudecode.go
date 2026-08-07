//go:build !no_claudecode

// Registers the Claude Code adapter into the process-wide runtime registry via
// its init().
package main

import _ "github.com/tianlinzz/agent-cli-gateway/adapters/claudecode"
