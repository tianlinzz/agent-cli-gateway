//go:build !linux

package nsjail

import (
	"context"
	"fmt"
	"runtime"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// platformPreflight fails closed on non-linux hosts: a real nsjail cannot
// run here, so the capability checks (version, namespaces, minimal jail) are
// impossible and the gateway must not serve unsandboxed workers. The real
// checks run on Linux CI (task 7); the fail-closed paths are unit-tested with
// stubs on this host.
func platformPreflight(_ context.Context, _ config.IsolationConfig) error {
	return fmt.Errorf("nsjail: preflight requires a linux host (running on %s); cannot verify the sandbox boundary", runtime.GOOS)
}
