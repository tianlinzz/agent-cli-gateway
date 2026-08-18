package nsjail

import (
	"strings"
	"testing"

	"github.com/tianlinzz/agent-cli-gateway/config"
)

// TestEnsureJailOwnership_Guards covers the no-op and fail-closed paths that
// are observable on any host (the actual chown only runs as euid 0):
//   - user namespace disabled → no-op (default non-root deployments never
//     touch ownership);
//   - a de-privileging map with uid/gid 0 is rejected instead of silently
//     chown-to-root;
//   - a well-formed config never errors on an existing path, root or not.
func TestEnsureJailOwnership_Guards(t *testing.T) {
	dir := t.TempDir()

	iso := config.DefaultGatewayConfig().Isolation
	if err := EnsureJailOwnership(dir, iso); err != nil {
		t.Errorf("enabled userns on existing dir: unexpected error: %v", err)
	}

	iso = config.DefaultGatewayConfig().Isolation
	iso.UserNamespace.Enabled = false
	if err := EnsureJailOwnership(dir, iso); err != nil {
		t.Errorf("disabled userns must be a no-op, got %v", err)
	}

	iso = config.DefaultGatewayConfig().Isolation
	iso.UserNamespace.UID = 0
	err := EnsureJailOwnership(dir, iso)
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Errorf("uid=0 map must fail closed mentioning uid, got %v", err)
	}
}
