//go:build !no_kimi

// Registers the Kimi adapter into the process-wide runtime registry via its
// init().
package main

import _ "github.com/tianlinzz/agent-cli-gateway/adapters/kimi"
