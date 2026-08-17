//go:build linux

package worker

import (
	"context"
	"os/exec"

	"github.com/tianlinzz/agent-cli-gateway/worker/nsjail"
)

// spawnJailed is the production (and dev) isolation path: it forks the REAL
// nsjail binary wrapping the worker executable. It is linux-only code —
// nsjail cannot run on darwin, where the fail-closed stub in spawn_other.go
// is compiled instead. A failure here is fail-closed: the caller surfaces the
// error and never falls back to a direct spawn.
func spawnJailed(ctx context.Context, spec spawnSpec) (*exec.Cmd, error) {
	// Defensive re-check even though StartSession validates: this function
	// must never fork a worker through a missing/non-executable nsjail.
	if err := nsjail.ValidateBinary(spec.nsjailBinary); err != nil {
		return nil, err
	}
	argv := buildNsjailCommand(spec.nsjailBinary, spec.profilePath, spec.workerExe)
	return startCommand(ctx, argv, spec.env, spec.logPath)
}
