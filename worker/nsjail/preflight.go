package nsjail

import (
	"context"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// Preflight verifies the nsjail sandbox boundary is available before the
// gateway serves requests. It fails closed: when isolation is required and
// the binary is missing/non-executable, or the platform cannot run the
// required namespaces/seccomp, it returns an error that the API layer maps to
// readiness 503. The gateway must NEVER serve unsandboxed workers.
//
// The cross-platform binary check runs everywhere. The capability checks
// (version pinning, minimal jail) are linux-only (preflight_linux.go); on
// other hosts they fail closed because a real nsjail cannot run there.
func Preflight(ctx context.Context, iso config.IsolationConfig) error {
	if !iso.Required {
		return nil // test profile: no isolation to verify
	}
	if err := ValidateBinary(iso.BinaryPath); err != nil {
		return err
	}
	return platformPreflight(ctx, iso)
}
